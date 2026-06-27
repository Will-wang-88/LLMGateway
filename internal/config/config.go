package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Auth          AuthConfig          `yaml:"auth"`
	Routing       RoutingConfig       `yaml:"routing"`
	HealthCheck   HealthCheckConfig   `yaml:"health_check"`
	RateLimit     RateLimitConfig     `yaml:"rate_limit"`
	Logging       LoggingConfig       `yaml:"logging"`
	Metrics       MetricsConfig       `yaml:"metrics"`
	Admin         AdminConfig         `yaml:"admin"`
	Storage       StorageConfig       `yaml:"storage"`
	Queue         QueueConfig         `yaml:"queue"`
	Tracing       TracingConfig       `yaml:"tracing"`
	Dashboard     DashboardConfig     `yaml:"dashboard"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Orchestration OrchestrationConfig `yaml:"orchestration"`
	Compression   CompressionConfig   `yaml:"compression"`

	Backends     []BackendConfig    `yaml:"backends"`
	Models       []ModelConfig      `yaml:"models"`
	ModelAliases []ModelAliasConfig `yaml:"model_aliases"`
	APIKeys      []APIKeyConfig     `yaml:"api_keys"`
	AdminUsers   []AdminUserConfig  `yaml:"admin_users"`
}

type NotificationsConfig struct {
	Email EmailNotifierConfig `yaml:"email" json:"email"`
}

type EmailNotifierConfig struct {
	Enabled    bool     `yaml:"enabled" json:"enabled"`
	SMTPHost   string   `yaml:"smtp_host" json:"smtp_host"`
	SMTPPort   int      `yaml:"smtp_port" json:"smtp_port"`
	Username   string   `yaml:"username" json:"username"`
	Password   string   `yaml:"password" json:"-"`
	From       string   `yaml:"from" json:"from"`
	To         []string `yaml:"to" json:"to"`
	UseTLS     bool     `yaml:"use_tls" json:"use_tls"`
	StartTLS   bool     `yaml:"start_tls" json:"start_tls"`
	CooldownMS int      `yaml:"cooldown_ms" json:"cooldown_ms"`
	NotifyOn   []string `yaml:"notify_on" json:"notify_on"`
}

type ServerConfig struct {
	Host                string `yaml:"host"`
	Port                int    `yaml:"port"`
	RequestBodyLimitMB  int    `yaml:"request_body_limit_mb"`
	DefaultTimeoutMS    int    `yaml:"default_timeout_ms"`
	StreamIdleTimeoutMS int    `yaml:"stream_idle_timeout_ms"`
	ReadHeaderTimeoutMS int    `yaml:"read_header_timeout_ms"`
	ShutdownTimeoutMS   int    `yaml:"shutdown_timeout_ms"`
	// TrustedProxies lists IPs / CIDRs that are allowed to set
	// X-Forwarded-For / X-Real-IP. Headers from any other peer are
	// ignored when resolving the client IP. Empty (default) means: never
	// trust forwarded headers.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

type AuthConfig struct {
	APIKeyHeader  string `yaml:"api_key_header"`
	APIKeyPrefix  string `yaml:"api_key_prefix"`
	HashAlgorithm string `yaml:"hash_algorithm"`
	HashSecret    string `yaml:"hash_secret"`
}

type RoutingConfig struct {
	DefaultPolicy      string `yaml:"default_policy" json:"default_policy"`
	ModelAliasEnabled  bool   `yaml:"model_alias_enabled" json:"model_alias_enabled"`
	UnknownFieldPolicy string `yaml:"unknown_field_policy" json:"unknown_field_policy"`
	// AllowDegradedBackends, when true, lets routing pick backends in the
	// "degraded" state (e.g. health probe returned 4xx). Default false:
	// degraded backends are excluded from routing because the gateway
	// has already observed an unrecoverable error from them.
	AllowDegradedBackends bool `yaml:"allow_degraded_backends" json:"allow_degraded_backends"`
}

type HealthCheckConfig struct {
	// Enabled lets operators turn off probing for a specific backend
	// without removing it from the catalog. nil = use the default.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Type selects the probe transport. Supported: "http" (default),
	// "tcp". The "completion" probe is reserved for a future change.
	Type             string `yaml:"type,omitempty" json:"type,omitempty"`
	IntervalMS       int    `yaml:"interval_ms" json:"interval_ms"`
	TimeoutMS        int    `yaml:"timeout_ms" json:"timeout_ms"`
	FailureThreshold int    `yaml:"failure_threshold" json:"failure_threshold"`
	SuccessThreshold int    `yaml:"success_threshold" json:"success_threshold"`
	Path             string `yaml:"path" json:"path"`
	// Method is the HTTP verb used by the http probe (default GET).
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	// Body is an optional request body for completion-style probes.
	Body string `yaml:"body,omitempty" json:"body,omitempty"`
}

type RateLimitConfig struct {
	Backend                  string `yaml:"backend"`
	DefaultRequestsPerMinute int    `yaml:"default_requests_per_minute"`
	DefaultConcurrentReq     int    `yaml:"default_concurrent_requests"`
	// RedisURL is consumed when Backend = "redis". Example:
	// "redis://:password@redis:6379/0". Multi-replica deployments
	// require this to keep limits consistent across pods.
	RedisURL    string `yaml:"redis_url"`
	RedisPrefix string `yaml:"redis_prefix"`
}

type LoggingConfig struct {
	Level                  string `yaml:"level" json:"level"`
	DefaultLogMetadata     bool   `yaml:"default_log_metadata" json:"default_log_metadata"`
	DefaultLogInput        bool   `yaml:"default_log_input" json:"default_log_input"`
	DefaultLogOutput       bool   `yaml:"default_log_output" json:"default_log_output"`
	DefaultLogRawRequest   bool   `yaml:"default_log_raw_request" json:"default_log_raw_request"`
	DefaultLogRawResponse  bool   `yaml:"default_log_raw_response" json:"default_log_raw_response"`
	DefaultLogStreamChunks bool   `yaml:"default_log_stream_chunks" json:"default_log_stream_chunks"`
}

type MetricsConfig struct {
	PrometheusEnabled bool   `yaml:"prometheus_enabled" json:"prometheus_enabled"`
	PrometheusPath    string `yaml:"prometheus_path" json:"prometheus_path"`
}

type AdminConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	BindToken string `yaml:"bind_token"`
}

type AdminUserConfig struct {
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordHash string `yaml:"password_hash"`
	Role         string `yaml:"role"` // super_admin, admin, operator, viewer, auditor
	Email        string `yaml:"email"`
}

type StorageConfig struct {
	Driver           string `yaml:"driver"` // memory (default), sqlite
	DSN              string `yaml:"dsn"`    // for sqlite: file path
	LogRetentionDays int    `yaml:"log_retention_days"`
}

type QueueConfig struct {
	Enabled        bool `yaml:"enabled" json:"enabled"`
	MaxQueueSize   int  `yaml:"max_queue_size" json:"max_queue_size"`
	QueueTimeoutMS int  `yaml:"queue_timeout_ms" json:"queue_timeout_ms"`
	PerModelLimit  int  `yaml:"per_model_limit" json:"per_model_limit"`
}

type TracingConfig struct {
	Enabled     bool    `yaml:"enabled" json:"enabled"`
	Endpoint    string  `yaml:"endpoint" json:"endpoint"`
	Service     string  `yaml:"service_name" json:"service_name"`
	SampleRatio float64 `yaml:"sample_ratio" json:"sample_ratio"`
}

type DashboardConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	BaseURL string `yaml:"base_url" json:"base_url"`
}

// OrchestrationConfig configures the Fugu-style model orchestration layer.
// It exposes one or two virtual models (a low-latency Tier-A router and an
// optional higher-quality Tier-B conductor) that fan a single request out
// to a pool of self-hosted worker models. See internal/orchestrator.
type OrchestrationConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// RouterModel is the virtual model name that triggers the Tier-A
	// (low-latency, single-worker) routing path. Clients call this name
	// like any other model.
	RouterModel string `yaml:"router_model" json:"router_model"`
	// ConductorModel is the virtual model name that triggers the Tier-B
	// (multi-step DAG) path. Leave empty to disable Tier-B.
	ConductorModel string `yaml:"conductor_model" json:"conductor_model"`
	// ConfidenceThreshold: when the Tier-A classifier's confidence for a
	// request falls below this value, the router escalates to Tier-B (if
	// configured) or to the strongest worker. Range 0..1. Default 0.55.
	ConfidenceThreshold float64 `yaml:"confidence_threshold" json:"confidence_threshold"`
	// MaxSteps caps the Tier-B workflow length (hard upper bound on the
	// number of worker calls in a single conductor run). Default 5.
	MaxSteps int `yaml:"max_steps" json:"max_steps"`
	// CostPenalty biases worker selection away from expensive workers when
	// scores are close: effective_score = strength - cost_penalty*cost.
	// 0 (default) disables the penalty.
	CostPenalty float64 `yaml:"cost_penalty" json:"cost_penalty"`
	// RequestTimeoutMS bounds a single worker call made by the
	// orchestrator. Default 120000.
	RequestTimeoutMS int `yaml:"request_timeout_ms" json:"request_timeout_ms"`
	// SecretLevelHeader names the inbound HTTP header carrying the request
	// data-sensitivity level (an integer). Workers whose secret_max_level
	// is below the request level are excluded from routing. Default
	// "X-Secret-Level".
	SecretLevelHeader string `yaml:"secret_level_header" json:"secret_level_header"`
	// Workers is the model pool the orchestrator can dispatch to.
	Workers []OrchestrationWorker `yaml:"workers" json:"workers"`
}

// OrchestrationWorker describes one member of the orchestration pool. Model
// must be an internal model name that resolves to one or more backends via
// the normal routing machinery.
type OrchestrationWorker struct {
	ID    string `yaml:"id" json:"id"`
	Model string `yaml:"model" json:"model"`
	// Tasks lists the classifier task labels this worker is preferred for
	// (e.g. code, reasoning, general, multilingual, zh, verify).
	Tasks []string `yaml:"tasks" json:"tasks"`
	// Strength is a 0..1 general-capability prior used to break ties and
	// to pick the "strongest" worker on low-confidence fallback.
	Strength float64 `yaml:"strength" json:"strength"`
	// Cost is a relative cost weight (e.g. GPU-seconds proxy) used by the
	// cost penalty. Higher = more expensive.
	Cost float64 `yaml:"cost" json:"cost"`
	// SecretMaxLevel is the highest request secret level this worker may
	// process. Self-hosted workers should set this high (e.g. 5); a cloud
	// worker added later should set it low so sensitive data never routes
	// to it. 0 is treated as "unlimited" (on-prem default).
	SecretMaxLevel int `yaml:"secret_max_level" json:"secret_max_level"`
	// Tags are free-form labels (informational; surfaced in metadata).
	Tags []string `yaml:"tags" json:"tags"`
}

type BackendConfig struct {
	ID                    string             `yaml:"id"`
	Name                  string             `yaml:"name"`
	BaseURL               string             `yaml:"base_url"`
	APIKey                string             `yaml:"api_key"`
	Enabled               bool               `yaml:"enabled"`
	Models                []string           `yaml:"models"`
	Weight                int                `yaml:"weight"`
	MaxConcurrentRequests int                `yaml:"max_concurrent_requests"`
	TimeoutMS             int                `yaml:"timeout_ms"`
	StreamIdleTimeoutMS   int                `yaml:"stream_idle_timeout_ms"`
	HealthCheck           *HealthCheckConfig `yaml:"health_check,omitempty"`
	Tags                  []string           `yaml:"tags"`
}

type ModelConfig struct {
	Name           string             `yaml:"name"`
	Type           string             `yaml:"type"`
	Enabled        bool               `yaml:"enabled"`
	ContextLength  int                `yaml:"context_length"`
	CapabilityMode string             `yaml:"capability_mode"`
	Capabilities   map[string]bool    `yaml:"capabilities"`
	RoutingPolicy  string             `yaml:"routing_policy"`
	Compression    *CompressionConfig `yaml:"compression,omitempty"`
}

// CompressionConfig controls the input-token compression pass. All fields are
// pointers so the three layers (global default <- model <- API key) overlay
// cleanly: a nil field inherits from the layer below. Resolve with Resolve.
type CompressionConfig struct {
	Enabled        *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MinInputTokens *int  `yaml:"min_input_tokens,omitempty" json:"min_input_tokens,omitempty"`
	LosslessOnly   *bool `yaml:"lossless_only,omitempty" json:"lossless_only,omitempty"`
	TokenBudget    *int  `yaml:"token_budget,omitempty" json:"token_budget,omitempty"`
}

// Resolve overlays an override on top of the receiver, returning a new config
// where each non-nil field of override wins. Either side may be nil.
func (c *CompressionConfig) Resolve(override *CompressionConfig) CompressionConfig {
	var out CompressionConfig
	if c != nil {
		out = *c
	}
	if override == nil {
		return out
	}
	if override.Enabled != nil {
		out.Enabled = override.Enabled
	}
	if override.MinInputTokens != nil {
		out.MinInputTokens = override.MinInputTokens
	}
	if override.LosslessOnly != nil {
		out.LosslessOnly = override.LosslessOnly
	}
	if override.TokenBudget != nil {
		out.TokenBudget = override.TokenBudget
	}
	return out
}

type ModelAliasConfig struct {
	Alias          string `yaml:"alias"`
	InternalModel  string `yaml:"internal_model"`
	ForwardingMode string `yaml:"forwarding_mode"`
	Enabled        bool   `yaml:"enabled"`
}

type APIKeyConfig struct {
	ID               string             `yaml:"id"`
	Name             string             `yaml:"name"`
	Key              string             `yaml:"key"`
	KeyPrefix        string             `yaml:"key_prefix"`
	KeyHash          string             `yaml:"key_hash"`
	Enabled          bool               `yaml:"enabled"`
	AllowedModels    []string           `yaml:"allowed_models"`
	DeniedModels     []string           `yaml:"denied_models"`
	AllowedClientIPs []string           `yaml:"allowed_client_ips"`
	DeniedClientIPs  []string           `yaml:"denied_client_ips"`
	RateLimit        *APIKeyRateLimit   `yaml:"rate_limit,omitempty"`
	Quota            *APIKeyQuota       `yaml:"quota,omitempty"`
	DelayMS          int                `yaml:"delay_ms"`
	Logging          *APIKeyLogging     `yaml:"logging,omitempty"`
	Compression      *CompressionConfig `yaml:"compression,omitempty"`
	ExpiresAt        string             `yaml:"expires_at"`
}

type APIKeyRateLimit struct {
	Enabled            bool  `yaml:"enabled" json:"enabled"`
	RequestsPerMinute  int   `yaml:"requests_per_minute" json:"requests_per_minute"`
	TokensPerMinute    int   `yaml:"tokens_per_minute" json:"tokens_per_minute"`
	RequestsPerDay     int64 `yaml:"requests_per_day" json:"requests_per_day,omitempty"`
	TokensPerDay       int64 `yaml:"tokens_per_day" json:"tokens_per_day,omitempty"`
	ConcurrentRequests int   `yaml:"concurrent_requests" json:"concurrent_requests"`
}

type APIKeyQuota struct {
	DailyRequests   int64 `yaml:"daily_requests" json:"daily_requests,omitempty"`
	DailyTokens     int64 `yaml:"daily_tokens" json:"daily_tokens,omitempty"`
	MonthlyRequests int64 `yaml:"monthly_requests" json:"monthly_requests,omitempty"`
	MonthlyTokens   int64 `yaml:"monthly_tokens" json:"monthly_tokens,omitempty"`
}

type APIKeyLogging struct {
	LogMetadata     bool `yaml:"log_metadata" json:"log_metadata"`
	LogInput        bool `yaml:"log_input" json:"log_input"`
	LogOutput       bool `yaml:"log_output" json:"log_output"`
	LogRawRequest   bool `yaml:"log_raw_request" json:"log_raw_request"`
	LogRawResponse  bool `yaml:"log_raw_response" json:"log_raw_response"`
	LogStreamChunks bool `yaml:"log_stream_chunks" json:"log_stream_chunks"`
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host:                "0.0.0.0",
			Port:                8080,
			RequestBodyLimitMB:  50,
			DefaultTimeoutMS:    120000,
			StreamIdleTimeoutMS: 30000,
			ReadHeaderTimeoutMS: 30000,
			ShutdownTimeoutMS:   30000,
		},
		Auth: AuthConfig{
			APIKeyHeader:  "Authorization",
			APIKeyPrefix:  "Bearer ",
			HashAlgorithm: "hmac_sha256",
		},
		Routing: RoutingConfig{
			DefaultPolicy:      "weighted_round_robin",
			ModelAliasEnabled:  true,
			UnknownFieldPolicy: "passthrough",
		},
		HealthCheck: HealthCheckConfig{
			IntervalMS:       5000,
			TimeoutMS:        2000,
			FailureThreshold: 3,
			SuccessThreshold: 2,
			Path:             "/models",
		},
		RateLimit: RateLimitConfig{
			Backend:                  "memory",
			DefaultRequestsPerMinute: 60,
			DefaultConcurrentReq:     10,
		},
		Logging: LoggingConfig{
			Level:              "info",
			DefaultLogMetadata: true,
		},
		Metrics: MetricsConfig{
			PrometheusEnabled: true,
			PrometheusPath:    "/metrics",
		},
		Admin: AdminConfig{
			Enabled: true,
		},
		Storage: StorageConfig{
			Driver:           "memory",
			LogRetentionDays: 30,
		},
		Queue: QueueConfig{
			Enabled:        false,
			MaxQueueSize:   1000,
			QueueTimeoutMS: 30000,
		},
		Tracing: TracingConfig{
			Enabled:     false,
			Service:     "llmgateway",
			SampleRatio: 1.0,
		},
		Dashboard: DashboardConfig{
			Enabled: true,
		},
		Orchestration: OrchestrationConfig{
			Enabled:             false,
			ConfidenceThreshold: 0.55,
			MaxSteps:            5,
			RequestTimeoutMS:    120000,
			SecretLevelHeader:   "X-Secret-Level",
		},
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid server.port: %d", c.Server.Port)
	}
	if c.Server.RequestBodyLimitMB <= 0 {
		c.Server.RequestBodyLimitMB = 50
	}
	for _, b := range c.Backends {
		if b.ID == "" {
			return fmt.Errorf("backend missing id")
		}
		if b.BaseURL == "" {
			return fmt.Errorf("backend %s missing base_url", b.ID)
		}
		if len(b.Models) == 0 {
			return fmt.Errorf("backend %s declares no models", b.ID)
		}
	}
	for _, m := range c.Models {
		switch m.CapabilityMode {
		case "", "passthrough", "declared", "strict":
		default:
			return fmt.Errorf("model %s: invalid capability_mode %q (must be passthrough/declared/strict)", m.Name, m.CapabilityMode)
		}
	}
	for _, a := range c.ModelAliases {
		switch a.ForwardingMode {
		case "", "use_internal", "keep_external":
		default:
			return fmt.Errorf("alias %s: invalid forwarding_mode %q (must be use_internal/keep_external)", a.Alias, a.ForwardingMode)
		}
	}
	if err := c.Orchestration.validate(); err != nil {
		return err
	}
	if err := c.validateOrchestrationRefs(); err != nil {
		return err
	}
	return nil
}

// validateOrchestrationRefs cross-checks the orchestration config against the
// rest of the catalog: a virtual model name must not collide with a real
// model / alias / backend-advertised model (else it would silently shadow
// it via the Handles() early-return in the request path), and every worker
// model must resolve to at least one backend.
func (c *Config) validateOrchestrationRefs() error {
	o := &c.Orchestration
	if !o.Enabled {
		return nil
	}
	realModels := make(map[string]struct{})
	for _, m := range c.Models {
		realModels[m.Name] = struct{}{}
	}
	for _, a := range c.ModelAliases {
		realModels[a.Alias] = struct{}{}
	}
	backendModels := make(map[string]struct{})
	for _, b := range c.Backends {
		for _, m := range b.Models {
			backendModels[m] = struct{}{}
			realModels[m] = struct{}{}
		}
	}
	for _, vm := range []struct{ kind, name string }{
		{"router_model", o.RouterModel},
		{"conductor_model", o.ConductorModel},
	} {
		if vm.name == "" {
			continue
		}
		if _, clash := realModels[vm.name]; clash {
			return fmt.Errorf("orchestration %s %q collides with a real model/alias/backend model; pick a distinct virtual name", vm.kind, vm.name)
		}
	}
	for i := range o.Workers {
		w := &o.Workers[i]
		if _, ok := backendModels[w.Model]; !ok {
			return fmt.Errorf("orchestration worker %s references model %q that no backend serves", w.ID, w.Model)
		}
	}
	return nil
}

func (o *OrchestrationConfig) validate() error {
	if !o.Enabled {
		return nil
	}
	if o.RouterModel == "" && o.ConductorModel == "" {
		return fmt.Errorf("orchestration enabled but neither router_model nor conductor_model is set")
	}
	if o.RouterModel != "" && o.RouterModel == o.ConductorModel {
		return fmt.Errorf("orchestration router_model and conductor_model must differ")
	}
	if len(o.Workers) == 0 {
		return fmt.Errorf("orchestration enabled but no workers configured")
	}
	if o.ConfidenceThreshold < 0 || o.ConfidenceThreshold > 1 {
		return fmt.Errorf("orchestration confidence_threshold must be in [0,1], got %v", o.ConfidenceThreshold)
	}
	if o.MaxSteps <= 0 {
		o.MaxSteps = 5
	}
	if o.MaxSteps > 5 {
		// Section 8: ≤5 steps is a hard upper bound (latency stacks).
		return fmt.Errorf("orchestration max_steps must be <= 5, got %d", o.MaxSteps)
	}
	if o.RequestTimeoutMS <= 0 {
		o.RequestTimeoutMS = 120000
	}
	if o.SecretLevelHeader == "" {
		o.SecretLevelHeader = "X-Secret-Level"
	}
	ids := make(map[string]struct{}, len(o.Workers))
	for i := range o.Workers {
		w := &o.Workers[i]
		if w.ID == "" {
			return fmt.Errorf("orchestration worker[%d] missing id", i)
		}
		if w.Model == "" {
			return fmt.Errorf("orchestration worker %s missing model", w.ID)
		}
		if _, dup := ids[w.ID]; dup {
			return fmt.Errorf("orchestration duplicate worker id %q", w.ID)
		}
		ids[w.ID] = struct{}{}
	}
	return nil
}

func (s *ServerConfig) ReadHeaderTimeout() time.Duration {
	if s.ReadHeaderTimeoutMS <= 0 {
		return 30 * time.Second
	}
	return time.Duration(s.ReadHeaderTimeoutMS) * time.Millisecond
}

func (s *ServerConfig) ShutdownTimeout() time.Duration {
	if s.ShutdownTimeoutMS <= 0 {
		return 30 * time.Second
	}
	return time.Duration(s.ShutdownTimeoutMS) * time.Millisecond
}
