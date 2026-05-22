package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
)

type BackendStatus string

const (
	StatusHealthy     BackendStatus = "healthy"
	StatusDegraded    BackendStatus = "degraded"
	StatusUnhealthy   BackendStatus = "unhealthy"
	StatusDisabled    BackendStatus = "disabled"
	StatusMaintenance BackendStatus = "maintenance"
	StatusUnknown     BackendStatus = "unknown"
)

type Backend struct {
	ID                    string
	Name                  string
	BaseURL               string
	APIKey                string
	Enabled               bool
	Models                []string
	Weight                int
	MaxConcurrentRequests int
	TimeoutMS             int
	StreamIdleTimeoutMS   int
	HealthCheck           *config.HealthCheckConfig
	Tags                  []string

	mu                sync.RWMutex
	status            BackendStatus
	failureStreak     int
	successStreak     int
	activeRequests    int64
	totalRequests     int64
	totalErrors       int64
	lastHealthCheckAt time.Time
	lastError         string
	lastLatencyMS     int64
}

func (b *Backend) Status() BackendStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.Enabled {
		return StatusDisabled
	}
	if b.status == "" {
		return StatusUnknown
	}
	return b.status
}

func (b *Backend) SetStatus(s BackendStatus) {
	b.mu.Lock()
	b.status = s
	b.mu.Unlock()
}

func (b *Backend) RecordHealthCheck(success bool, latencyMS int64, errMsg string) (changed bool, newStatus BackendStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastHealthCheckAt = time.Now()
	b.lastLatencyMS = latencyMS
	prev := b.status
	threshFail := 3
	threshSucc := 2
	if b.HealthCheck != nil {
		if b.HealthCheck.FailureThreshold > 0 {
			threshFail = b.HealthCheck.FailureThreshold
		}
		if b.HealthCheck.SuccessThreshold > 0 {
			threshSucc = b.HealthCheck.SuccessThreshold
		}
	}
	if success {
		b.successStreak++
		b.failureStreak = 0
		b.lastError = ""
		if prev != StatusHealthy && b.successStreak >= threshSucc {
			b.status = StatusHealthy
		}
		if prev == "" || prev == StatusUnknown {
			b.status = StatusHealthy
		}
	} else {
		b.failureStreak++
		b.successStreak = 0
		b.lastError = errMsg
		if prev != StatusUnhealthy && b.failureStreak >= threshFail {
			b.status = StatusUnhealthy
		}
		if prev == "" || prev == StatusUnknown {
			if b.failureStreak >= threshFail {
				b.status = StatusUnhealthy
			}
		}
	}
	return prev != b.status, b.status
}

func (b *Backend) AcquireSlot() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxConcurrentRequests > 0 && b.activeRequests >= int64(b.MaxConcurrentRequests) {
		return false
	}
	b.activeRequests++
	b.totalRequests++
	return true
}

func (b *Backend) ReleaseSlot(success bool) {
	b.mu.Lock()
	if b.activeRequests > 0 {
		b.activeRequests--
	}
	if !success {
		b.totalErrors++
	}
	b.mu.Unlock()
}

func (b *Backend) ActiveRequests() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.activeRequests
}

func (b *Backend) Stats() (active, total, errors int64, lastErr string, lastLatencyMS int64, lastCheck time.Time) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.activeRequests, b.totalRequests, b.totalErrors, b.lastError, b.lastLatencyMS, b.lastHealthCheckAt
}

func (b *Backend) SupportsModel(model string) bool {
	for _, m := range b.Models {
		if m == model {
			return true
		}
	}
	return false
}

type Model struct {
	Name           string
	Type           string
	Enabled        bool
	ContextLength  int
	CapabilityMode string
	Capabilities   map[string]bool
	RoutingPolicy  string
}

type ModelAlias struct {
	Alias          string
	InternalModel  string
	ForwardingMode string
	Enabled        bool
}

type APIKey struct {
	ID            string
	Name          string
	KeyPrefix     string
	KeyHash       string
	Enabled       bool
	AllowedModels []string
	DeniedModels  []string
	RateLimit     *config.APIKeyRateLimit
	Quota         *config.APIKeyQuota
	DelayMS       int
	Logging       *config.APIKeyLogging
	ExpiresAt     time.Time

	mu          sync.RWMutex
	lastUsedAt  time.Time
	totalRequests int64
	totalTokens   int64
}

func (k *APIKey) Touch(tokens int64) {
	k.mu.Lock()
	k.lastUsedAt = time.Now()
	k.totalRequests++
	k.totalTokens += tokens
	k.mu.Unlock()
}

func (k *APIKey) Stats() (lastUsed time.Time, totalRequests, totalTokens int64) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.lastUsedAt, k.totalRequests, k.totalTokens
}

func (k *APIKey) ModelAllowed(model string) bool {
	for _, d := range k.DeniedModels {
		if matchPattern(d, model) {
			return false
		}
	}
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, a := range k.AllowedModels {
		if matchPattern(a, model) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, value string) bool {
	if pattern == "*" || pattern == value {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(value, prefix)
	}
	return false
}

type Store struct {
	mu           sync.RWMutex
	backends     map[string]*Backend
	models       map[string]*Model
	modelAliases map[string]*ModelAlias
	apiKeys      map[string]*APIKey
	keyByHash    map[string]*APIKey
	hashSecret   []byte
}

func New(hashSecret string) *Store {
	return &Store{
		backends:     make(map[string]*Backend),
		models:       make(map[string]*Model),
		modelAliases: make(map[string]*ModelAlias),
		apiKeys:      make(map[string]*APIKey),
		keyByHash:    make(map[string]*APIKey),
		hashSecret:   []byte(hashSecret),
	}
}

func (s *Store) HashKey(key string) string {
	if len(s.hashSecret) == 0 {
		h := sha256.Sum256([]byte(key))
		return hex.EncodeToString(h[:])
	}
	m := hmac.New(sha256.New, s.hashSecret)
	m.Write([]byte(key))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Store) LoadFromConfig(cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bc := range cfg.Backends {
		b := &Backend{
			ID:                    bc.ID,
			Name:                  bc.Name,
			BaseURL:               strings.TrimRight(bc.BaseURL, "/"),
			APIKey:                bc.APIKey,
			Enabled:               bc.Enabled,
			Models:                append([]string(nil), bc.Models...),
			Weight:                bc.Weight,
			MaxConcurrentRequests: bc.MaxConcurrentRequests,
			TimeoutMS:             bc.TimeoutMS,
			StreamIdleTimeoutMS:   bc.StreamIdleTimeoutMS,
			HealthCheck:           bc.HealthCheck,
			Tags:                  append([]string(nil), bc.Tags...),
			status:                StatusUnknown,
		}
		if b.Weight <= 0 {
			b.Weight = 1
		}
		s.backends[b.ID] = b
	}
	for _, mc := range cfg.Models {
		m := &Model{
			Name:           mc.Name,
			Type:           mc.Type,
			Enabled:        mc.Enabled,
			ContextLength:  mc.ContextLength,
			CapabilityMode: mc.CapabilityMode,
			Capabilities:   mc.Capabilities,
			RoutingPolicy:  mc.RoutingPolicy,
		}
		if m.Type == "" {
			m.Type = "chat"
		}
		if m.CapabilityMode == "" {
			m.CapabilityMode = "passthrough"
		}
		s.models[m.Name] = m
	}
	for _, mac := range cfg.ModelAliases {
		ma := &ModelAlias{
			Alias:          mac.Alias,
			InternalModel:  mac.InternalModel,
			ForwardingMode: mac.ForwardingMode,
			Enabled:        mac.Enabled,
		}
		if ma.ForwardingMode == "" {
			ma.ForwardingMode = "use_internal"
		}
		s.modelAliases[ma.Alias] = ma
	}
	for _, kc := range cfg.APIKeys {
		k := &APIKey{
			ID:            kc.ID,
			Name:          kc.Name,
			KeyPrefix:     kc.KeyPrefix,
			KeyHash:       kc.KeyHash,
			Enabled:       kc.Enabled,
			AllowedModels: append([]string(nil), kc.AllowedModels...),
			DeniedModels:  append([]string(nil), kc.DeniedModels...),
			RateLimit:     kc.RateLimit,
			Quota:         kc.Quota,
			DelayMS:       kc.DelayMS,
			Logging:       kc.Logging,
		}
		if kc.Key != "" {
			k.KeyHash = s.HashKey(kc.Key)
			if k.KeyPrefix == "" && len(kc.Key) >= 8 {
				k.KeyPrefix = kc.Key[:8]
			}
		}
		if kc.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, kc.ExpiresAt); err == nil {
				k.ExpiresAt = t
			}
		}
		if k.ID == "" {
			k.ID = k.KeyPrefix
		}
		s.apiKeys[k.ID] = k
		if k.KeyHash != "" {
			s.keyByHash[k.KeyHash] = k
		}
	}
	return nil
}

var (
	ErrNotFound = errors.New("not found")
)

func (s *Store) Backends() []*Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Backend, 0, len(s.backends))
	for _, b := range s.backends {
		out = append(out, b)
	}
	return out
}

func (s *Store) Backend(id string) (*Backend, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.backends[id]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

func (s *Store) UpsertBackend(b *Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.Weight <= 0 {
		b.Weight = 1
	}
	b.BaseURL = strings.TrimRight(b.BaseURL, "/")
	s.backends[b.ID] = b
}

func (s *Store) DeleteBackend(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.backends, id)
}

func (s *Store) BackendsForModel(model string) []*Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Backend, 0)
	for _, b := range s.backends {
		if b.SupportsModel(model) {
			out = append(out, b)
		}
	}
	return out
}

func (s *Store) Models() []*Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Model, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, m)
	}
	return out
}

func (s *Store) Model(name string) (*Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.models[name]
	return m, ok
}

func (s *Store) UpsertModel(m *Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models[m.Name] = m
}

func (s *Store) DeleteModel(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.models, name)
}

func (s *Store) ResolveAlias(name string) (internal string, forwardName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.modelAliases[name]
	if !ok || !a.Enabled {
		return name, name
	}
	switch a.ForwardingMode {
	case "keep_external":
		return a.InternalModel, name
	default:
		return a.InternalModel, a.InternalModel
	}
}

func (s *Store) ModelAliases() []*ModelAlias {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ModelAlias, 0, len(s.modelAliases))
	for _, a := range s.modelAliases {
		out = append(out, a)
	}
	return out
}

func (s *Store) UpsertModelAlias(a *ModelAlias) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ForwardingMode == "" {
		a.ForwardingMode = "use_internal"
	}
	s.modelAliases[a.Alias] = a
}

func (s *Store) DeleteModelAlias(alias string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.modelAliases, alias)
}

func (s *Store) APIKeys() []*APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*APIKey, 0, len(s.apiKeys))
	for _, k := range s.apiKeys {
		out = append(out, k)
	}
	return out
}

func (s *Store) APIKey(id string) (*APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.apiKeys[id]
	return k, ok
}

func (s *Store) APIKeyByRaw(raw string) (*APIKey, bool) {
	hash := s.HashKey(raw)
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keyByHash[hash]
	return k, ok
}

func (s *Store) UpsertAPIKey(k *APIKey, rawKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rawKey != "" {
		k.KeyHash = s.HashKey(rawKey)
		if k.KeyPrefix == "" && len(rawKey) >= 8 {
			k.KeyPrefix = rawKey[:8]
		}
	}
	if k.ID == "" {
		k.ID = k.KeyPrefix
	}
	s.apiKeys[k.ID] = k
	if k.KeyHash != "" {
		s.keyByHash[k.KeyHash] = k
	}
}

func (s *Store) DeleteAPIKey(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.apiKeys[id]; ok {
		delete(s.keyByHash, k.KeyHash)
	}
	delete(s.apiKeys, id)
}
