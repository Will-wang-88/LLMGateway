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

// MarkDegraded records that a probe returned a non-fatal failure (e.g.
// 4xx) so we know the backend is reachable but should be treated with
// caution. Counters are reset (degraded is a soft state).
func (b *Backend) MarkDegraded(latencyMS int64, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = StatusDegraded
	b.lastHealthCheckAt = time.Now()
	b.lastLatencyMS = latencyMS
	b.lastError = reason
	b.failureStreak = 0
	b.successStreak = 0
}

// MarkMaintenance puts a backend into the maintenance state.
// Maintenance is set via the admin API and persists until cleared.
func (b *Backend) MarkMaintenance(on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if on {
		b.status = StatusMaintenance
	} else {
		b.status = StatusUnknown
		b.failureStreak = 0
		b.successStreak = 0
	}
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
		// success_threshold is the canonical gate. unknown is treated
		// the same as any non-healthy state — operators expect "the
		// probe must succeed N times before I trust this backend".
		// (The previous shortcut for unknown is removed.)
		if prev != StatusHealthy && b.successStreak >= threshSucc {
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
	ID               string
	Name             string
	KeyPrefix        string
	KeyHash          string
	Enabled          bool
	AllowedModels    []string
	DeniedModels     []string
	AllowedClientIPs []string
	DeniedClientIPs  []string
	RateLimit        *config.APIKeyRateLimit
	Quota            *config.APIKeyQuota
	DelayMS          int
	Logging          *config.APIKeyLogging
	ExpiresAt        time.Time

	mu          sync.RWMutex
	lastUsedAt  time.Time
	totalRequests int64
	totalTokens   int64
}

// TouchRequest records that a request from this key was admitted/completed.
// Token totals are updated separately via AddTokens (or via Touch for
// callers that have both pieces of information at once).
func (k *APIKey) TouchRequest() {
	k.mu.Lock()
	k.lastUsedAt = time.Now()
	k.totalRequests++
	k.mu.Unlock()
}

// AddTokens accumulates token usage without bumping the request counter.
func (k *APIKey) AddTokens(tokens int64) {
	if tokens <= 0 {
		return
	}
	k.mu.Lock()
	k.lastUsedAt = time.Now()
	k.totalTokens += tokens
	k.mu.Unlock()
}

// Touch is retained for callers that have both signals at once.
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

// ModelAllowedResolved checks model permission for both the requested
// (external) name and the resolved internal model. Deny is enforced on
// either name; allow requires at least one name to match. This closes the
// alias-bypass hole where a client uses an alias whose internal model is
// in DeniedModels.
func (k *APIKey) ModelAllowedResolved(requested, internal string) bool {
	for _, d := range k.DeniedModels {
		if matchPattern(d, requested) || (internal != requested && matchPattern(d, internal)) {
			return false
		}
	}
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, a := range k.AllowedModels {
		if matchPattern(a, requested) {
			return true
		}
		if internal != requested && matchPattern(a, internal) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == value {
		return true
	}
	if !strings.ContainsRune(pattern, '*') {
		return false
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	// Anchor at start unless pattern begins with '*'.
	if parts[0] != "" {
		if !strings.HasPrefix(value, parts[0]) {
			return false
		}
		pos = len(parts[0])
	}
	for i := 1; i < len(parts)-1; i++ {
		seg := parts[i]
		if seg == "" {
			continue
		}
		idx := strings.Index(value[pos:], seg)
		if idx == -1 {
			return false
		}
		pos += idx + len(seg)
	}
	// Anchor at end unless pattern ends with '*'.
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(value[pos:], last) {
			return false
		}
	}
	return true
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

// HashKey produces an HMAC-SHA256 digest of the raw key using the store's
// configured secret. The secret is mandatory in production - without one,
// HMAC degrades to plain SHA256 which would let an attacker pre-compute
// hashes from a leaked db. Callers should ensure store.New is given a
// non-empty secret; if not, a fixed default sentinel is mixed in so the
// hash is not just sha256(key), but operators are warned.
func (s *Store) HashKey(key string) string {
	secret := s.hashSecret
	if len(secret) == 0 {
		secret = []byte("llmgateway-default-hmac-secret-replace-via-LLMGATEWAY_HASH_SECRET")
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(key))
	return hex.EncodeToString(m.Sum(nil))
}

// HasExplicitHashSecret reports whether the store was constructed with a
// non-empty HMAC secret.
func (s *Store) HasExplicitHashSecret() bool { return len(s.hashSecret) > 0 }

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
		// Negative weight is invalid; treat as 1. Zero is preserved as a
		// drain-mode signal so routing skips the backend.
		if b.Weight < 0 {
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
			ID:               kc.ID,
			Name:             kc.Name,
			KeyPrefix:        kc.KeyPrefix,
			KeyHash:          kc.KeyHash,
			Enabled:          kc.Enabled,
			AllowedModels:    append([]string(nil), kc.AllowedModels...),
			DeniedModels:     append([]string(nil), kc.DeniedModels...),
			AllowedClientIPs: append([]string(nil), kc.AllowedClientIPs...),
			DeniedClientIPs:  append([]string(nil), kc.DeniedClientIPs...),
			RateLimit:        kc.RateLimit,
			Quota:            kc.Quota,
			DelayMS:          kc.DelayMS,
			Logging:          kc.Logging,
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
	if b.Weight < 0 {
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

// ResolveAlias walks alias chains up to maxAliasHops with a cycle guard.
// Returns (internalModel, modelToSendUpstream). modelToSendUpstream is the
// caller-visible alias if forwarding_mode is keep_external on the first hop,
// otherwise it's the fully-resolved internal model.
func (s *Store) ResolveAlias(name string) (internal string, forwardName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	const maxAliasHops = 8
	seen := map[string]bool{}
	current := name
	keepExternal := false
	for i := 0; i < maxAliasHops; i++ {
		a, ok := s.modelAliases[current]
		if !ok || !a.Enabled {
			break
		}
		if seen[current] {
			break // cycle
		}
		seen[current] = true
		if i == 0 && a.ForwardingMode == "keep_external" {
			keepExternal = true
		}
		current = a.InternalModel
	}
	if keepExternal {
		return current, name
	}
	return current, current
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
