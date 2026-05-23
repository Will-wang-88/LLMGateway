package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Auth        AuthConfig        `yaml:"auth"`
	Routing     RoutingConfig     `yaml:"routing"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	RateLimit   RateLimitConfig   `yaml:"rate_limit"`
	Logging     LoggingConfig     `yaml:"logging"`
	Metrics     MetricsConfig     `yaml:"metrics"`
	Admin       AdminConfig       `yaml:"admin"`
	Storage     StorageConfig     `yaml:"storage"`
	Queue       QueueConfig       `yaml:"queue"`
	Tracing     TracingConfig     `yaml:"tracing"`
	Dashboard   DashboardConfig   `yaml:"dashboard"`

	Backends     []BackendConfig    `yaml:"backends"`
	Models       []ModelConfig      `yaml:"models"`
	ModelAliases []ModelAliasConfig `yaml:"model_aliases"`
	APIKeys      []APIKeyConfig     `yaml:"api_keys"`
	AdminUsers   []AdminUserConfig  `yaml:"admin_users"`
}

type ServerConfig struct {
	Host                 string   `yaml:"host"`
	Port                 int      `yaml:"port"`
	RequestBodyLimitMB   int      `yaml:"request_body_limit_mb"`
	DefaultTimeoutMS     int      `yaml:"default_timeout_ms"`
	StreamIdleTimeoutMS  int      `yaml:"stream_idle_timeout_ms"`
	ReadHeaderTimeoutMS  int      `yaml:"read_header_timeout_ms"`
	ShutdownTimeoutMS    int      `yaml:"shutdown_timeout_ms"`
	// TrustedProxies lists IPs / CIDRs that are allowed to set
	// X-Forwarded-For / X-Real-IP. Headers from any other peer are
	// ignored when resolving the client IP. Empty (default) means: never
	// trust forwarded headers.
	TrustedProxies       []string `yaml:"trusted_proxies"`
}

type AuthConfig struct {
	APIKeyHeader   string `yaml:"api_key_header"`
	APIKeyPrefix   string `yaml:"api_key_prefix"`
	HashAlgorithm  string `yaml:"hash_algorithm"`
	HashSecret     string `yaml:"hash_secret"`
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
	IntervalMS       int    `yaml:"interval_ms" json:"interval_ms"`
	TimeoutMS        int    `yaml:"timeout_ms" json:"timeout_ms"`
	FailureThreshold int    `yaml:"failure_threshold" json:"failure_threshold"`
	SuccessThreshold int    `yaml:"success_threshold" json:"success_threshold"`
	Path             string `yaml:"path" json:"path"`
}

type RateLimitConfig struct {
	Backend                  string `yaml:"backend"`
	DefaultRequestsPerMinute int    `yaml:"default_requests_per_minute"`
	DefaultConcurrentReq     int    `yaml:"default_concurrent_requests"`
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
	Enabled    bool   `yaml:"enabled"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
	BindToken  string `yaml:"bind_token"`
}

type AdminUserConfig struct {
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordHash string `yaml:"password_hash"`
	Role         string `yaml:"role"` // super_admin, admin, operator, viewer, auditor
	Email        string `yaml:"email"`
}

type StorageConfig struct {
	Driver string `yaml:"driver"` // memory (default), sqlite
	DSN    string `yaml:"dsn"`    // for sqlite: file path
	LogRetentionDays int `yaml:"log_retention_days"`
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
	Name           string            `yaml:"name"`
	Type           string            `yaml:"type"`
	Enabled        bool              `yaml:"enabled"`
	ContextLength  int               `yaml:"context_length"`
	CapabilityMode string            `yaml:"capability_mode"`
	Capabilities   map[string]bool   `yaml:"capabilities"`
	RoutingPolicy  string            `yaml:"routing_policy"`
}

type ModelAliasConfig struct {
	Alias          string `yaml:"alias"`
	InternalModel  string `yaml:"internal_model"`
	ForwardingMode string `yaml:"forwarding_mode"`
	Enabled        bool   `yaml:"enabled"`
}

type APIKeyConfig struct {
	ID                string                 `yaml:"id"`
	Name              string                 `yaml:"name"`
	Key               string                 `yaml:"key"`
	KeyPrefix         string                 `yaml:"key_prefix"`
	KeyHash           string                 `yaml:"key_hash"`
	Enabled           bool                   `yaml:"enabled"`
	AllowedModels     []string               `yaml:"allowed_models"`
	DeniedModels      []string               `yaml:"denied_models"`
	AllowedClientIPs  []string               `yaml:"allowed_client_ips"`
	DeniedClientIPs   []string               `yaml:"denied_client_ips"`
	RateLimit         *APIKeyRateLimit       `yaml:"rate_limit,omitempty"`
	Quota             *APIKeyQuota           `yaml:"quota,omitempty"`
	DelayMS           int                    `yaml:"delay_ms"`
	Logging           *APIKeyLogging         `yaml:"logging,omitempty"`
	ExpiresAt         string                 `yaml:"expires_at"`
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
