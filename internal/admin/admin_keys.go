package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// getAPIKey returns the metadata for a single API key (never the raw key).
func (s *Server) getAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	k, ok := s.store.APIKey(id)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("API key not found", "api_key_not_found"))
		return
	}
	last, totalReqs, totalToks := k.Stats()
	out := map[string]any{
		"id":                 k.ID,
		"name":               k.Name,
		"key_prefix":         k.KeyPrefix,
		"enabled":            k.Enabled,
		"allowed_models":     k.AllowedModels,
		"denied_models":      k.DeniedModels,
		"allowed_client_ips": k.AllowedClientIPs,
		"denied_client_ips":  k.DeniedClientIPs,
		"rate_limit":         k.RateLimit,
		"quota":              k.Quota,
		"delay_ms":           k.DelayMS,
		"logging":            k.Logging,
		"total_requests":     totalReqs,
		"total_tokens":       totalToks,
	}
	if !last.IsZero() {
		out["last_used_at"] = last.UTC().Format(time.RFC3339)
	}
	if !k.ExpiresAt.IsZero() {
		out["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// patchAPIKey updates an existing API key's policy. The raw key cannot be
// updated this way (use rotate). All fields are optional; only fields that
// are explicitly present in the body are modified.
func (s *Server) patchAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_keys", w, r) {
		return
	}
	id := r.PathValue("id")
	k, ok := s.store.APIKey(id)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("API key not found", "api_key_not_found"))
		return
	}
	body := struct {
		Name             *string                 `json:"name"`
		Enabled          *bool                   `json:"enabled"`
		AllowedModels    *[]string               `json:"allowed_models"`
		DeniedModels     *[]string               `json:"denied_models"`
		AllowedClientIPs *[]string               `json:"allowed_client_ips"`
		DeniedClientIPs  *[]string               `json:"denied_client_ips"`
		RateLimit        *config.APIKeyRateLimit `json:"rate_limit"`
		Quota            *config.APIKeyQuota     `json:"quota"`
		DelayMS          *int                    `json:"delay_ms"`
		Logging          *config.APIKeyLogging   `json:"logging"`
		ExpiresAt        *string                 `json:"expires_at"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	oldVal := summarizeKey(k)

	if body.Name != nil {
		k.Name = *body.Name
	}
	if body.Enabled != nil {
		k.Enabled = *body.Enabled
	}
	if body.AllowedModels != nil {
		k.AllowedModels = *body.AllowedModels
	}
	if body.DeniedModels != nil {
		k.DeniedModels = *body.DeniedModels
	}
	if body.AllowedClientIPs != nil {
		k.AllowedClientIPs = *body.AllowedClientIPs
	}
	if body.DeniedClientIPs != nil {
		k.DeniedClientIPs = *body.DeniedClientIPs
	}
	if body.RateLimit != nil {
		k.RateLimit = body.RateLimit
	}
	if body.Quota != nil {
		k.Quota = body.Quota
	}
	if body.DelayMS != nil {
		k.DelayMS = *body.DelayMS
	}
	if body.Logging != nil {
		k.Logging = body.Logging
	}
	if body.ExpiresAt != nil {
		if *body.ExpiresAt == "" {
			k.ExpiresAt = time.Time{}
		} else if t, err := time.Parse(time.RFC3339, *body.ExpiresAt); err == nil {
			k.ExpiresAt = t
		}
	}
	// In-place mutation is already visible to other goroutines, but make
	// sure the store's key-by-hash index reflects metadata changes by
	// re-upserting (no rawKey passed so hash stays the same).
	s.store.UpsertAPIKey(k, "")
	s.audit(r, "api_key.update", "api_key", k.ID, oldVal, summarizeKey(k))
	writeJSON(w, http.StatusOK, summarizeKey(k))
}

func (s *Server) enableAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_keys", w, r) {
		return
	}
	s.setAPIKeyEnabled(w, r, true)
}

func (s *Server) disableAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_keys", w, r) {
		return
	}
	s.setAPIKeyEnabled(w, r, false)
}

func (s *Server) setAPIKeyEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id := r.PathValue("id")
	k, ok := s.store.APIKey(id)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("API key not found", "api_key_not_found"))
		return
	}
	oldVal := summarizeKey(k)
	k.Enabled = enabled
	s.store.UpsertAPIKey(k, "")
	action := "api_key.enable"
	if !enabled {
		action = "api_key.disable"
	}
	s.audit(r, action, "api_key", id, oldVal, summarizeKey(k))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": enabled})
}

// rotateAPIKey generates a new random key, replaces the stored hash, and
// returns the plaintext exactly once. The old key stops working immediately.
func (s *Server) rotateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_keys", w, r) {
		return
	}
	id := r.PathValue("id")
	k, ok := s.store.APIKey(id)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("API key not found", "api_key_not_found"))
		return
	}
	body := struct {
		Key string `json:"key"`
	}{}
	_ = json.NewDecoder(r.Body).Decode(&body) // ignore decode err, body is optional
	raw := body.Key
	if raw == "" {
		raw = generateAPIKey()
	}
	// Drop the previous hash from the index by deleting then re-upserting.
	s.store.DeleteAPIKey(k.ID)
	k.KeyPrefix = ""
	k.KeyHash = ""
	s.store.UpsertAPIKey(k, raw)
	s.audit(r, "api_key.rotate", "api_key", id, nil, map[string]any{"id": id, "key_prefix": k.KeyPrefix})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"key_prefix": k.KeyPrefix,
		"key":        raw,
		"warning":    "Save this key. The previous key has been invalidated and this value will not be shown again.",
	})
}

// toggleMaintenance puts a backend in or out of maintenance mode.
// Body: {"on": true|false}. While in maintenance, routing skips the backend.
func (s *Server) toggleMaintenance(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("backend_toggle", w, r) {
		return
	}
	id := r.PathValue("id")
	b, err := s.store.Backend(id)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, proxy.NotFound("Backend not found", "backend_not_found"))
		return
	}
	body := struct {
		On bool `json:"on"`
	}{On: true}
	_ = json.NewDecoder(r.Body).Decode(&body)
	b.MarkMaintenance(body.On)
	action := "backend.maintenance_on"
	if !body.On {
		action = "backend.maintenance_off"
	}
	s.audit(r, action, "backend", id, nil, map[string]any{"on": body.On})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": string(b.Status())})
}

// statsRange returns aggregated stats over the given window via the log store.
// Query params: since=RFC3339, until=RFC3339. Defaults to the last 24h.
func (s *Server) statsRange(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeJSON(w, http.StatusOK, &logstore.Stats{
			ByModel:    map[string]logstore.ModelStat{},
			ByBackend:  map[string]logstore.BackendStat{},
			ByAPIKey:   map[string]logstore.KeyStat{},
			ByClientIP: map[string]logstore.ClientIPStat{},
		})
		return
	}
	since := time.Now().Add(-24 * time.Hour)
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}
	stats, err := s.logs.StatsSince(r.Context(), since)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, proxy.InternalError("stats: "+err.Error(), "stats_failed"))
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func summarizeKey(k *store.APIKey) map[string]any {
	last, totalReqs, totalToks := k.Stats()
	out := map[string]any{
		"id":                 k.ID,
		"name":               k.Name,
		"key_prefix":         k.KeyPrefix,
		"enabled":            k.Enabled,
		"allowed_models":     k.AllowedModels,
		"denied_models":      k.DeniedModels,
		"allowed_client_ips": k.AllowedClientIPs,
		"denied_client_ips":  k.DeniedClientIPs,
		"rate_limit":         k.RateLimit,
		"quota":              k.Quota,
		"delay_ms":           k.DelayMS,
		"logging":            k.Logging,
		"total_requests":     totalReqs,
		"total_tokens":       totalToks,
	}
	if !last.IsZero() {
		out["last_used_at"] = last.UTC().Format(time.RFC3339)
	}
	if !k.ExpiresAt.IsZero() {
		out["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}

func generateAPIKey() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "sk-" + hex.EncodeToString(b[:])
}
