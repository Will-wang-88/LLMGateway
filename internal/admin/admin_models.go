package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	m, ok := s.store.Model(name)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Model not found", "model_not_found"))
		return
	}
	writeJSON(w, http.StatusOK, modelEntry(m))
}

// patchModel updates only the fields present in the request body.
// Properties left unset remain unchanged.
func (s *Server) patchModel(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_models", w, r) {
		return
	}
	name := r.PathValue("name")
	m, ok := s.store.Model(name)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Model not found", "model_not_found"))
		return
	}
	body := struct {
		Type           *string                   `json:"type"`
		Enabled        *bool                     `json:"enabled"`
		ContextLength  *int                      `json:"context_length"`
		CapabilityMode *string                   `json:"capability_mode"`
		Capabilities   *map[string]bool          `json:"capabilities"`
		RoutingPolicy  *string                   `json:"routing_policy"`
		Compression    *config.CompressionConfig `json:"compression"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	oldVal := modelEntry(m)
	if body.Type != nil {
		m.Type = *body.Type
	}
	if body.Enabled != nil {
		m.Enabled = *body.Enabled
	}
	if body.ContextLength != nil {
		m.ContextLength = *body.ContextLength
	}
	if body.CapabilityMode != nil {
		if !validCapabilityMode(*body.CapabilityMode) {
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"capability_mode must be one of passthrough/declared/strict",
				"invalid_capability_mode",
			))
			return
		}
		m.CapabilityMode = *body.CapabilityMode
	}
	if body.Capabilities != nil {
		m.Capabilities = *body.Capabilities
	}
	if body.RoutingPolicy != nil {
		m.RoutingPolicy = *body.RoutingPolicy
	}
	if body.Compression != nil {
		m.Compression = body.Compression
	}
	s.store.UpsertModel(m)
	s.audit(r, "model.update", "model", m.Name, oldVal, modelEntry(m))
	writeJSON(w, http.StatusOK, modelEntry(m))
}

// patchAlias updates only the fields present in the request body.
func (s *Server) patchAlias(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_models", w, r) {
		return
	}
	alias := r.PathValue("alias")
	a, ok := s.lookupAlias(alias)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Alias not found", "alias_not_found"))
		return
	}
	body := struct {
		InternalModel  *string `json:"internal_model"`
		ForwardingMode *string `json:"forwarding_mode"`
		Enabled        *bool   `json:"enabled"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	oldVal := aliasEntry(a)
	if body.InternalModel != nil {
		a.InternalModel = *body.InternalModel
	}
	if body.ForwardingMode != nil {
		if !validForwardingMode(*body.ForwardingMode) {
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"forwarding_mode must be use_internal or keep_external",
				"invalid_forwarding_mode",
			))
			return
		}
		a.ForwardingMode = *body.ForwardingMode
	}
	if body.Enabled != nil {
		a.Enabled = *body.Enabled
	}
	s.store.UpsertModelAlias(a)
	s.audit(r, "alias.update", "alias", a.Alias, oldVal, aliasEntry(a))
	writeJSON(w, http.StatusOK, aliasEntry(a))
}

func (s *Server) lookupAlias(name string) (*store.ModelAlias, bool) {
	for _, a := range s.store.ModelAliases() {
		if a.Alias == name {
			return a, true
		}
	}
	return nil, false
}

func modelEntry(m *store.Model) map[string]any {
	return map[string]any{
		"name":            m.Name,
		"type":            m.Type,
		"enabled":         m.Enabled,
		"context_length":  m.ContextLength,
		"capability_mode": m.CapabilityMode,
		"capabilities":    m.Capabilities,
		"routing_policy":  m.RoutingPolicy,
		"compression":     m.Compression,
	}
}

func aliasEntry(a *store.ModelAlias) map[string]any {
	return map[string]any{
		"alias":           a.Alias,
		"internal_model":  a.InternalModel,
		"forwarding_mode": a.ForwardingMode,
		"enabled":         a.Enabled,
	}
}

// adminMetrics returns aggregated counters/gauges for the dashboard
// without scraping Prometheus. Latency percentiles come from the log
// store if available; otherwise they are reported as 0.
func (s *Server) adminMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("view_logs", w, r) {
		return
	}
	out := map[string]any{
		"backends_total":   len(s.store.Backends()),
		"backends_healthy": healthyBackends(s.store),
		"models_total":     len(s.store.Models()),
		"api_keys_total":   len(s.store.APIKeys()),
		"active_requests":  totalActiveRequests(s.store),
	}
	if s.logs != nil {
		since := time.Now().Add(-15 * time.Minute)
		if v := r.URL.Query().Get("since"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				since = t
			}
		}
		stats, err := s.logs.StatsSince(r.Context(), since)
		if err == nil {
			out["window_since"] = since.UTC().Format(time.RFC3339)
			out["total_requests"] = stats.TotalRequests
			out["success_total"] = stats.SuccessTotal
			out["error_total"] = stats.ErrorTotal
			out["total_tokens"] = stats.TotalTokens
			out["compressed_requests"] = stats.CompressedRequests
			out["tokens_saved"] = stats.TokensSaved
			out["avg_compression_ratio"] = stats.AvgCompressionRatio
			elapsed := time.Since(since).Seconds()
			if elapsed > 0 {
				out["qps"] = float64(stats.TotalRequests) / elapsed
				out["tokens_per_second"] = float64(stats.TotalTokens) / elapsed
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) statsModels(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	since := windowSince(r)
	st, err := s.logs.StatsSince(r.Context(), since)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, proxy.InternalError("stats: "+err.Error(), "stats_failed"))
		return
	}
	type row struct {
		Model    string `json:"model"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
		Tokens   int64  `json:"tokens"`
	}
	out := make([]row, 0, len(st.ByModel))
	for k, v := range st.ByModel {
		out = append(out, row{Model: k, Requests: v.Requests, Errors: v.Errors, Tokens: v.Tokens})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out, "window_since": since.UTC().Format(time.RFC3339)})
}

func (s *Server) statsBackends(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	since := windowSince(r)
	st, err := s.logs.StatsSince(r.Context(), since)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, proxy.InternalError("stats: "+err.Error(), "stats_failed"))
		return
	}
	type row struct {
		Backend  string `json:"backend"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
		Tokens   int64  `json:"tokens"`
	}
	out := make([]row, 0, len(st.ByBackend))
	for k, v := range st.ByBackend {
		out = append(out, row{Backend: k, Requests: v.Requests, Errors: v.Errors, Tokens: v.Tokens})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out, "window_since": since.UTC().Format(time.RFC3339)})
}

func (s *Server) statsAPIKeys(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	since := windowSince(r)
	st, err := s.logs.StatsSince(r.Context(), since)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, proxy.InternalError("stats: "+err.Error(), "stats_failed"))
		return
	}
	type row struct {
		APIKey   string `json:"api_key"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
		Tokens   int64  `json:"tokens"`
	}
	out := make([]row, 0, len(st.ByAPIKey))
	for k, v := range st.ByAPIKey {
		out = append(out, row{APIKey: k, Requests: v.Requests, Errors: v.Errors, Tokens: v.Tokens})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out, "window_since": since.UTC().Format(time.RFC3339)})
}

func windowSince(r *http.Request) time.Time {
	since := time.Now().Add(-24 * time.Hour)
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}
	return since
}

func healthyBackends(s *store.Store) int {
	n := 0
	for _, b := range s.Backends() {
		if b.Status() == store.StatusHealthy {
			n++
		}
	}
	return n
}

func totalActiveRequests(s *store.Store) int64 {
	var n int64
	for _, b := range s.Backends() {
		a, _, _, _, _, _ := b.Stats()
		n += a
	}
	return n
}

// validCapabilityMode is the single source of truth for the allowed
// values across POST / PATCH / config-load paths.
func validCapabilityMode(s string) bool {
	switch s {
	case "passthrough", "declared", "strict":
		return true
	}
	return false
}

// validForwardingMode mirrors validCapabilityMode for model aliases.
func validForwardingMode(s string) bool {
	switch s {
	case "use_internal", "keep_external":
		return true
	}
	return false
}

// Force compile-time use of logstore types so go-vet stays happy when
// downstream is empty.
var _ = logstore.Stats{}
