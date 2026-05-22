package admin

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/will-wang-88/llmgateway/internal/backend"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// Server hosts the /admin/* HTTP API for managing the gateway at runtime.
// Authentication uses a static bearer token (cfg.Admin.BindToken) or basic auth
// (cfg.Admin.Username + Password). The token is read once at startup; in
// production deployments this should be sourced from a secret manager.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	health  *backend.HealthChecker
	logger  *logging.Logger
}

func NewServer(cfg *config.Config, s *store.Store, hc *backend.HealthChecker, log *logging.Logger) *Server {
	return &Server{cfg: cfg, store: s, health: hc, logger: log}
}

func (s *Server) Register(mux *http.ServeMux) {
	auth := s.authMiddleware
	mux.Handle("GET /admin/backends", auth(http.HandlerFunc(s.listBackends)))
	mux.Handle("POST /admin/backends", auth(http.HandlerFunc(s.createBackend)))
	mux.Handle("GET /admin/backends/{id}", auth(http.HandlerFunc(s.getBackend)))
	mux.Handle("PATCH /admin/backends/{id}", auth(http.HandlerFunc(s.patchBackend)))
	mux.Handle("DELETE /admin/backends/{id}", auth(http.HandlerFunc(s.deleteBackend)))
	mux.Handle("POST /admin/backends/{id}/enable", auth(http.HandlerFunc(s.enableBackend)))
	mux.Handle("POST /admin/backends/{id}/disable", auth(http.HandlerFunc(s.disableBackend)))
	mux.Handle("POST /admin/backends/{id}/health-check", auth(http.HandlerFunc(s.healthCheck)))

	mux.Handle("GET /admin/models", auth(http.HandlerFunc(s.listModels)))
	mux.Handle("POST /admin/models", auth(http.HandlerFunc(s.upsertModel)))
	mux.Handle("DELETE /admin/models/{name}", auth(http.HandlerFunc(s.deleteModel)))

	mux.Handle("GET /admin/model-aliases", auth(http.HandlerFunc(s.listAliases)))
	mux.Handle("POST /admin/model-aliases", auth(http.HandlerFunc(s.upsertAlias)))
	mux.Handle("DELETE /admin/model-aliases/{alias}", auth(http.HandlerFunc(s.deleteAlias)))

	mux.Handle("GET /admin/api-keys", auth(http.HandlerFunc(s.listAPIKeys)))
	mux.Handle("POST /admin/api-keys", auth(http.HandlerFunc(s.createAPIKey)))
	mux.Handle("DELETE /admin/api-keys/{id}", auth(http.HandlerFunc(s.deleteAPIKey)))

	mux.Handle("GET /admin/stats/overview", auth(http.HandlerFunc(s.statsOverview)))
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Admin.Enabled {
			proxy.WriteError(w, http.StatusForbidden, proxy.PermissionError("Admin API is disabled", "admin_disabled"))
			return
		}
		if s.cfg.Admin.BindToken != "" {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized("Missing admin token", "invalid_admin_token"))
				return
			}
			provided := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.Admin.BindToken)) != 1 {
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized("Invalid admin token", "invalid_admin_token"))
				return
			}
		} else if s.cfg.Admin.Username != "" {
			u, p, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.Admin.Username)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Admin.Password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="admin"`)
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized("Invalid admin credentials", "invalid_admin_credentials"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) listBackends(w http.ResponseWriter, _ *http.Request) {
	type stat struct {
		ID                    string   `json:"id"`
		Name                  string   `json:"name"`
		BaseURL               string   `json:"base_url"`
		Enabled               bool     `json:"enabled"`
		Status                string   `json:"status"`
		Models                []string `json:"models"`
		Weight                int      `json:"weight"`
		MaxConcurrentRequests int      `json:"max_concurrent_requests"`
		ActiveRequests        int64    `json:"active_requests"`
		TotalRequests         int64    `json:"total_requests"`
		TotalErrors           int64    `json:"total_errors"`
		LastError             string   `json:"last_error,omitempty"`
		LastLatencyMS         int64    `json:"last_latency_ms"`
		LastHealthCheck       string   `json:"last_health_check,omitempty"`
		Tags                  []string `json:"tags"`
	}
	out := make([]stat, 0)
	for _, b := range s.store.Backends() {
		active, total, errors, lastErr, lastLat, lastT := b.Stats()
		row := stat{
			ID:                    b.ID,
			Name:                  b.Name,
			BaseURL:               b.BaseURL,
			Enabled:               b.Enabled,
			Status:                string(b.Status()),
			Models:                b.Models,
			Weight:                b.Weight,
			MaxConcurrentRequests: b.MaxConcurrentRequests,
			ActiveRequests:        active,
			TotalRequests:         total,
			TotalErrors:           errors,
			LastError:             lastErr,
			LastLatencyMS:         lastLat,
			Tags:                  b.Tags,
		}
		if !lastT.IsZero() {
			row.LastHealthCheck = lastT.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

type backendBody struct {
	ID                    string                    `json:"id"`
	Name                  string                    `json:"name"`
	BaseURL               string                    `json:"base_url"`
	APIKey                string                    `json:"api_key"`
	Enabled               *bool                     `json:"enabled"`
	Models                []string                  `json:"models"`
	Weight                int                       `json:"weight"`
	MaxConcurrentRequests int                       `json:"max_concurrent_requests"`
	TimeoutMS             int                       `json:"timeout_ms"`
	StreamIdleTimeoutMS   int                       `json:"stream_idle_timeout_ms"`
	HealthCheck           *config.HealthCheckConfig `json:"health_check,omitempty"`
	Tags                  []string                  `json:"tags"`
}

func (s *Server) createBackend(w http.ResponseWriter, r *http.Request) {
	var b backendBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	if b.ID == "" || b.BaseURL == "" || len(b.Models) == 0 {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("id, base_url and models are required", "missing_fields"))
		return
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	bk := &store.Backend{
		ID:                    b.ID,
		Name:                  b.Name,
		BaseURL:               strings.TrimRight(b.BaseURL, "/"),
		APIKey:                b.APIKey,
		Enabled:               enabled,
		Models:                b.Models,
		Weight:                b.Weight,
		MaxConcurrentRequests: b.MaxConcurrentRequests,
		TimeoutMS:             b.TimeoutMS,
		StreamIdleTimeoutMS:   b.StreamIdleTimeoutMS,
		HealthCheck:           b.HealthCheck,
		Tags:                  b.Tags,
	}
	s.store.UpsertBackend(bk)
	s.health.CheckOnce(bk)
	writeJSON(w, http.StatusCreated, map[string]any{"id": b.ID, "status": "created"})
}

func (s *Server) getBackend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.Backend(id)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Backend not found", "backend_not_found"))
		return
	}
	active, total, errs, lastErr, lat, lastT := b.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                      b.ID,
		"name":                    b.Name,
		"base_url":                b.BaseURL,
		"enabled":                 b.Enabled,
		"status":                  string(b.Status()),
		"models":                  b.Models,
		"weight":                  b.Weight,
		"max_concurrent_requests": b.MaxConcurrentRequests,
		"active_requests":         active,
		"total_requests":          total,
		"total_errors":            errs,
		"last_error":              lastErr,
		"last_latency_ms":         lat,
		"last_health_check":       lastT.UTC().Format(time.RFC3339),
		"tags":                    b.Tags,
	})
}

func (s *Server) patchBackend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.Backend(id)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Backend not found", "backend_not_found"))
		return
	}
	var body backendBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	if body.Name != "" {
		b.Name = body.Name
	}
	if body.BaseURL != "" {
		b.BaseURL = strings.TrimRight(body.BaseURL, "/")
	}
	if body.APIKey != "" {
		b.APIKey = body.APIKey
	}
	if body.Enabled != nil {
		b.Enabled = *body.Enabled
	}
	if len(body.Models) > 0 {
		b.Models = body.Models
	}
	if body.Weight > 0 {
		b.Weight = body.Weight
	}
	if body.MaxConcurrentRequests > 0 {
		b.MaxConcurrentRequests = body.MaxConcurrentRequests
	}
	if body.TimeoutMS > 0 {
		b.TimeoutMS = body.TimeoutMS
	}
	if body.StreamIdleTimeoutMS > 0 {
		b.StreamIdleTimeoutMS = body.StreamIdleTimeoutMS
	}
	if body.HealthCheck != nil {
		b.HealthCheck = body.HealthCheck
	}
	if body.Tags != nil {
		b.Tags = body.Tags
	}
	s.store.UpsertBackend(b)
	writeJSON(w, http.StatusOK, map[string]any{"id": b.ID, "status": "updated"})
}

func (s *Server) deleteBackend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.DeleteBackend(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enableBackend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.Backend(id)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Backend not found", "backend_not_found"))
		return
	}
	b.Enabled = true
	s.store.UpsertBackend(b)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": true})
}

func (s *Server) disableBackend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.Backend(id)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Backend not found", "backend_not_found"))
		return
	}
	b.Enabled = false
	s.store.UpsertBackend(b)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": false})
}

func (s *Server) healthCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.Backend(id)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Backend not found", "backend_not_found"))
		return
	}
	s.health.CheckOnce(b)
	_, _, _, lastErr, lat, lastT := b.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               b.ID,
		"status":           string(b.Status()),
		"last_error":       lastErr,
		"last_latency_ms":  lat,
		"last_health_check": lastT.UTC().Format(time.RFC3339),
	})
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Name           string          `json:"name"`
		Type           string          `json:"type"`
		Enabled        bool            `json:"enabled"`
		ContextLength  int             `json:"context_length"`
		CapabilityMode string          `json:"capability_mode"`
		Capabilities   map[string]bool `json:"capabilities,omitempty"`
		RoutingPolicy  string          `json:"routing_policy"`
	}
	out := make([]entry, 0)
	for _, m := range s.store.Models() {
		out = append(out, entry{
			Name: m.Name, Type: m.Type, Enabled: m.Enabled, ContextLength: m.ContextLength,
			CapabilityMode: m.CapabilityMode, Capabilities: m.Capabilities, RoutingPolicy: m.RoutingPolicy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

type modelBody struct {
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	Enabled        *bool           `json:"enabled"`
	ContextLength  int             `json:"context_length"`
	CapabilityMode string          `json:"capability_mode"`
	Capabilities   map[string]bool `json:"capabilities"`
	RoutingPolicy  string          `json:"routing_policy"`
}

func (s *Server) upsertModel(w http.ResponseWriter, r *http.Request) {
	var b modelBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	if b.Name == "" {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("name is required", "missing_name"))
		return
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	if b.Type == "" {
		b.Type = "chat"
	}
	if b.CapabilityMode == "" {
		b.CapabilityMode = "passthrough"
	}
	s.store.UpsertModel(&store.Model{
		Name: b.Name, Type: b.Type, Enabled: enabled, ContextLength: b.ContextLength,
		CapabilityMode: b.CapabilityMode, Capabilities: b.Capabilities, RoutingPolicy: b.RoutingPolicy,
	})
	writeJSON(w, http.StatusOK, map[string]any{"name": b.Name, "status": "ok"})
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	s.store.DeleteModel(r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listAliases(w http.ResponseWriter, _ *http.Request) {
	out := make([]map[string]any, 0)
	for _, a := range s.store.ModelAliases() {
		out = append(out, map[string]any{
			"alias":          a.Alias,
			"internal_model": a.InternalModel,
			"forwarding_mode": a.ForwardingMode,
			"enabled":        a.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

type aliasBody struct {
	Alias          string `json:"alias"`
	InternalModel  string `json:"internal_model"`
	ForwardingMode string `json:"forwarding_mode"`
	Enabled        *bool  `json:"enabled"`
}

func (s *Server) upsertAlias(w http.ResponseWriter, r *http.Request) {
	var b aliasBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	if b.Alias == "" || b.InternalModel == "" {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("alias and internal_model are required", "missing_fields"))
		return
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	if b.ForwardingMode == "" {
		b.ForwardingMode = "use_internal"
	}
	s.store.UpsertModelAlias(&store.ModelAlias{
		Alias: b.Alias, InternalModel: b.InternalModel, ForwardingMode: b.ForwardingMode, Enabled: enabled,
	})
	writeJSON(w, http.StatusOK, map[string]any{"alias": b.Alias, "status": "ok"})
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	s.store.DeleteModelAlias(r.PathValue("alias"))
	w.WriteHeader(http.StatusNoContent)
}

type apiKeyBody struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Key           string                  `json:"key"`
	Enabled       *bool                   `json:"enabled"`
	AllowedModels []string                `json:"allowed_models"`
	DeniedModels  []string                `json:"denied_models"`
	RateLimit     *config.APIKeyRateLimit `json:"rate_limit,omitempty"`
	Quota         *config.APIKeyQuota     `json:"quota,omitempty"`
	DelayMS       int                     `json:"delay_ms"`
	Logging       *config.APIKeyLogging   `json:"logging,omitempty"`
	ExpiresAt     string                  `json:"expires_at"`
}

func (s *Server) listAPIKeys(w http.ResponseWriter, _ *http.Request) {
	out := make([]map[string]any, 0)
	for _, k := range s.store.APIKeys() {
		last, totalReqs, totalToks := k.Stats()
		entry := map[string]any{
			"id":             k.ID,
			"name":           k.Name,
			"key_prefix":     k.KeyPrefix,
			"enabled":        k.Enabled,
			"allowed_models": k.AllowedModels,
			"denied_models":  k.DeniedModels,
			"rate_limit":     k.RateLimit,
			"quota":          k.Quota,
			"delay_ms":       k.DelayMS,
			"logging":        k.Logging,
			"total_requests": totalReqs,
			"total_tokens":   totalToks,
		}
		if !last.IsZero() {
			entry["last_used_at"] = last.UTC().Format(time.RFC3339)
		}
		if !k.ExpiresAt.IsZero() {
			entry["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var b apiKeyBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	if b.Key == "" {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("key is required (raw key shown once; only hash is stored)", "missing_key"))
		return
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	k := &store.APIKey{
		ID:            b.ID,
		Name:          b.Name,
		Enabled:       enabled,
		AllowedModels: b.AllowedModels,
		DeniedModels:  b.DeniedModels,
		RateLimit:     b.RateLimit,
		Quota:         b.Quota,
		DelayMS:       b.DelayMS,
		Logging:       b.Logging,
	}
	if b.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, b.ExpiresAt); err == nil {
			k.ExpiresAt = t
		}
	}
	s.store.UpsertAPIKey(k, b.Key)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         k.ID,
		"key_prefix": k.KeyPrefix,
		"key":        b.Key,
		"warning":    "Save this key. It will not be shown again.",
	})
}

func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	s.store.DeleteAPIKey(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) statsOverview(w http.ResponseWriter, _ *http.Request) {
	type backendSummary struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		Active        int64  `json:"active_requests"`
		Total         int64  `json:"total_requests"`
		Errors        int64  `json:"total_errors"`
		LastLatencyMS int64  `json:"last_latency_ms"`
	}
	var totalReq, totalErr, totalActive int64
	healthy := 0
	bs := make([]backendSummary, 0)
	for _, b := range s.store.Backends() {
		active, total, errs, _, lat, _ := b.Stats()
		bs = append(bs, backendSummary{
			ID: b.ID, Status: string(b.Status()), Active: active,
			Total: total, Errors: errs, LastLatencyMS: lat,
		})
		totalReq += total
		totalErr += errs
		totalActive += active
		if b.Status() == store.StatusHealthy {
			healthy++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backends_total":   len(bs),
		"backends_healthy": healthy,
		"api_keys_total":   len(s.store.APIKeys()),
		"models_total":     len(s.store.Models()),
		"active_requests":  totalActive,
		"total_requests":   totalReq,
		"total_errors":     totalErr,
		"backends":         bs,
	})
}
