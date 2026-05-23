package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/will-wang-88/llmgateway/internal/admin"
	"github.com/will-wang-88/llmgateway/internal/auth"
	"github.com/will-wang-88/llmgateway/internal/backend"
	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/handlers"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/netutil"
	"github.com/will-wang-88/llmgateway/internal/notify"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/redis/go-redis/v9"

	"github.com/will-wang-88/llmgateway/internal/queue"
	"github.com/will-wang-88/llmgateway/internal/quota"
	"github.com/will-wang-88/llmgateway/internal/ratelimit"
	"github.com/will-wang-88/llmgateway/internal/store"
	"github.com/will-wang-88/llmgateway/internal/tracing"
	"github.com/will-wang-88/llmgateway/web"
)

const (
	envConfigPath   = "LLMGATEWAY_CONFIG"
	envHashSecret   = "LLMGATEWAY_HASH_SECRET"
	envAdminToken   = "LLMGATEWAY_ADMIN_TOKEN"
	envListenAddr   = "LLMGATEWAY_LISTEN"
	envSMTPPassword = "LLMGATEWAY_SMTP_PASSWORD"
)

// applyEnvOverrides mutates cfg in-place from well-known env vars. It is
// called before --healthcheck so the probe and the server agree on
// host:port and any other env-driven settings.
func applyEnvOverrides(cfg *config.Config) {
	if v := os.Getenv(envHashSecret); v != "" {
		cfg.Auth.HashSecret = v
	}
	if v := os.Getenv(envAdminToken); v != "" {
		cfg.Admin.BindToken = v
	}
	if v := os.Getenv(envListenAddr); v != "" {
		if host, port, ok := splitHostPort(v); ok {
			cfg.Server.Host = host
			cfg.Server.Port = port
		}
	}
	// SMTP password is sensitive; we strongly prefer env over the YAML.
	// Both LLMGATEWAY_SMTP_PASSWORD and SMTP_PASSWORD are accepted so
	// existing operator playbooks keep working.
	if v := os.Getenv(envSMTPPassword); v != "" {
		cfg.Notifications.Email.Password = v
	} else if v := os.Getenv("SMTP_PASSWORD"); v != "" {
		cfg.Notifications.Email.Password = v
	}
}

func main() {
	configPath := flag.String("config", envOr(envConfigPath, "config/gateway.yaml"), "Path to YAML config file")
	healthcheck := flag.Bool("healthcheck", false, "Probe /healthz on the configured listener and exit 0/1 (for Docker HEALTHCHECK)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	// Env overrides must run before --healthcheck so the probe targets
	// the same port the running gateway is bound to (LLMGATEWAY_LISTEN
	// can change host/port at runtime).
	applyEnvOverrides(cfg)

	if *healthcheck {
		os.Exit(runHealthCheck(cfg))
	}

	logger := logging.New(os.Stdout, cfg.Logging.Level)
	logging.SetDefault(logger)

	logger.Info("starting llm gateway", logging.F(
		"listen", fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		"backends", len(cfg.Backends),
		"models", len(cfg.Models),
		"api_keys", len(cfg.APIKeys),
	))

	s := store.New(cfg.Auth.HashSecret)
	if err := s.LoadFromConfig(cfg); err != nil {
		logger.Error("failed to load config into store", logging.F("error", err.Error()))
		os.Exit(1)
	}
	if !s.HasExplicitHashSecret() {
		logger.Warn("auth.hash_secret is not set - API keys are being hashed with a default HMAC secret; set LLMGATEWAY_HASH_SECRET in production", nil)
	}

	m := metrics.New()
	p := proxy.New(logger, m)
	bal := balancer.New()
	var rl ratelimit.Backend
	switch strings.ToLower(cfg.RateLimit.Backend) {
	case "redis":
		opts, err := redis.ParseURL(cfg.RateLimit.RedisURL)
		if err != nil {
			logger.Error("failed to parse rate_limit.redis_url", logging.F("error", err.Error()))
			os.Exit(1)
		}
		rdb := redis.NewClient(opts)
		rl = ratelimit.NewRedisLimiter(rdb, cfg.RateLimit.RedisPrefix)
		logger.Info("rate limit backend: redis", logging.F("prefix", cfg.RateLimit.RedisPrefix))
	default:
		rl = ratelimit.New()
		if cfg.RateLimit.Backend != "" && cfg.RateLimit.Backend != "memory" {
			logger.Warn("unknown rate_limit.backend, defaulting to memory", logging.F("got", cfg.RateLimit.Backend))
		}
	}
	cc := ratelimit.NewConcurrency()
	qm := quota.New()
	qq := queue.New(cfg.Queue.QueueTimeoutMS, cfg.Queue.MaxQueueSize, cfg.Queue.PerModelLimit)

	// Optional persistent log store.
	var ls logstore.Store
	switch cfg.Storage.Driver {
	case "", "memory":
		ls = logstore.NewMemory(50000)
	case "sqlite":
		sl, err := logstore.OpenSQLite(cfg.Storage.DSN)
		if err != nil {
			logger.Error("failed to open sqlite log store", logging.F("error", err.Error()))
			os.Exit(1)
		}
		ls = sl
		if cfg.Storage.LogRetentionDays > 0 {
			go func() {
				t := time.NewTicker(6 * time.Hour)
				defer t.Stop()
				for range t.C {
					retention := time.Duration(cfg.Storage.LogRetentionDays) * 24 * time.Hour
					_ = sl.Purge(context.Background(), retention)
				}
			}()
		}
	case "postgres", "postgresql":
		dsn := cfg.Storage.DSN
		if v := os.Getenv("LLMGATEWAY_PG_DSN"); v != "" {
			dsn = v
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			logger.Error("failed to open postgres", logging.F("error", err.Error()))
			os.Exit(1)
		}
		pg, err := logstore.OpenPostgres(db)
		if err != nil {
			logger.Error("failed to migrate postgres", logging.F("error", err.Error()))
			os.Exit(1)
		}
		ls = pg
		if cfg.Storage.LogRetentionDays > 0 {
			go func() {
				t := time.NewTicker(6 * time.Hour)
				defer t.Stop()
				for range t.C {
					retention := time.Duration(cfg.Storage.LogRetentionDays) * 24 * time.Hour
					_ = pg.Purge(context.Background(), retention)
				}
			}()
		}
	default:
		logger.Error("unsupported storage driver", logging.F("driver", cfg.Storage.Driver))
		os.Exit(1)
	}
	defer ls.Close()

	tr := tracing.New(cfg.Tracing, logger)
	defer tr.Stop()

	notifier := notify.New(cfg.Notifications, logger)
	notifier.Start()
	defer notifier.Stop()
	hostname := notify.Hostname()

	hc := backend.NewHealthChecker(s, cfg.HealthCheck, logger, func(b *store.Backend, status store.BackendStatus) {
		v := 0.0
		if status == store.StatusHealthy {
			v = 1.0
		}
		m.BackendStatus.WithLabelValues(b.ID).Set(v)
	})
	hc.AddObserver(func(b *store.Backend, prev, next store.BackendStatus, latencyMS int64, errMsg string) {
		notifier.Notify(notify.Event{
			BackendID: b.ID, BackendName: b.Name, BackendURL: b.BaseURL,
			Prev: prev, Next: next, LatencyMS: latencyMS, Error: errMsg,
			Models: b.Models, Hostname: hostname, At: time.Now().UTC(),
		})
	})
	hc.Start()
	defer hc.Stop()

	h := handlers.New(cfg, s, p, bal, rl, cc, logger, m).
		WithLogStore(ls).
		WithQuota(qm).
		WithQueue(qq).
		WithTracer(tr)
	ipExtractor := netutil.NewExtractor(cfg.Server.TrustedProxies)
	adminSrv := admin.NewServer(cfg, s, hc, logger).
		WithLogStore(ls).
		WithQuota(qm).
		WithNotifier(notifier).
		WithClientIPExtractor(ipExtractor)
	adminSrv.Users().LoadFromConfig(cfg.AdminUsers)

	mux := http.NewServeMux()

	// Health and meta endpoints (no auth).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		for _, b := range s.Backends() {
			if !b.Enabled {
				continue
			}
			if b.Status() == store.StatusHealthy || b.Status() == store.StatusUnknown {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ready\n"))
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("no healthy backends\n"))
	})

	if cfg.Metrics.PrometheusEnabled {
		path := cfg.Metrics.PrometheusPath
		if path == "" {
			path = "/metrics"
		}
		mux.Handle("GET "+path, promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	}

	// Build /v1/* handlers wrapped with API-key auth.
	authn := auth.New(s, cfg.Auth.APIKeyHeader, cfg.Auth.APIKeyPrefix).
		WithClientIPExtractor(ipExtractor).
		WithLogStore(ls)
	v1Auth := authn.Middleware(func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/v1/")
	})

	mux.Handle("GET /v1/models", v1Auth(http.HandlerFunc(h.ListModels)))
	mux.Handle("POST /v1/chat/completions", v1Auth(h.Forward("/chat/completions")))
	mux.Handle("POST /v1/completions", v1Auth(h.Forward("/completions")))
	mux.Handle("POST /v1/embeddings", v1Auth(h.Forward("/embeddings")))
	mux.Handle("POST /v1/responses", v1Auth(h.Forward("/responses")))
	mux.Handle("POST /v1/audio/transcriptions", v1Auth(h.ForwardMultipart("/audio/transcriptions")))
	mux.Handle("POST /v1/audio/translations", v1Auth(h.ForwardMultipart("/audio/translations")))
	mux.Handle("POST /v1/audio/speech", v1Auth(h.Forward("/audio/speech")))

	// Admin API.
	adminSrv.Register(mux)

	// Web dashboard (static assets + index.html).
	if cfg.Dashboard.Enabled {
		web.Register(mux)
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:           withRequestLogging(logger, mux),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server stopped", logging.F("error", err.Error()))
			os.Exit(1)
		}
	case sig := <-stopCh:
		logger.Info("shutdown signal received", logging.F("signal", sig.String()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout())
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", logging.F("error", err.Error()))
	}
	logger.Info("shutdown complete", nil)
}

// runHealthCheck performs a lightweight HTTP GET against /healthz on the
// listener address the gateway would itself bind to. It's invoked via
// `llmgateway --healthcheck` so docker-compose / k8s probes can verify
// liveness without a shell.
func runHealthCheck(cfg *config.Config) int {
	host := cfg.Server.Host
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d/healthz", host, cfg.Server.Port)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitHostPort(s string) (string, int, bool) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil || portStr == "" {
		return "", 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, false
	}
	return host, port, true
}

func withRequestLogging(logger *logging.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		// Skip noisy paths.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			return
		}
		logger.Debug("http request", logging.F(
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"latency_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.status = 200
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush makes the recorder transparent for SSE.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
