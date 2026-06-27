package handlers

import (
	"net/http"

	"github.com/will-wang-88/llmgateway/internal/compress"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// compressibleEndpoints are the message-carrying endpoints worth compressing.
// Embeddings/audio payloads are not tool-result conversations.
var compressibleEndpoints = map[string]bool{
	"/chat/completions": true,
	"/responses":        true,
	"/completions":      true,
}

// maybeCompress resolves the effective per-request compression policy
// (global default <- model <- API key) and, if enabled and the estimated input
// size exceeds the threshold, compresses *body in place. It returns the
// resulting stats (nil if compression did not run). On any failure it is a safe
// no-op, leaving *body unchanged.
func (h *Handler) maybeCompress(internalModel string, k *store.APIKey, endpoint string, body *[]byte) *compress.Stats {
	if !compressibleEndpoints[endpoint] {
		return nil
	}

	var model *store.Model
	if h.store != nil {
		if m, ok := h.store.Model(internalModel); ok {
			model = m
		}
	}

	// Layered overlay: global default, then model, then API key.
	base := h.cfg.Compression
	merged := (&base).Resolve(modelCompression(model))
	merged = (&merged).Resolve(keyCompression(k))

	if merged.Enabled == nil || !*merged.Enabled {
		return nil
	}
	minTok := 0
	if merged.MinInputTokens != nil {
		minTok = *merged.MinInputTokens
	}
	if minTok <= 0 {
		return nil // a threshold of 0 means "off" — require an explicit gate
	}
	if compress.EstimateTokens(*body) <= minTok {
		return nil // below threshold: not worth compressing
	}

	ccfg := compress.DefaultConfig()
	ccfg.Enabled = true
	if merged.LosslessOnly != nil {
		ccfg.LosslessOnly = *merged.LosslessOnly
	}
	switch {
	case merged.TokenBudget != nil && *merged.TokenBudget > 0:
		ccfg.TokenBudget = *merged.TokenBudget
	case model != nil && model.ContextLength > 0:
		// Default the lossy budget to a fraction of the context window so the
		// conservative-lossy stage engages before the hard ceiling rather than
		// only when a request would already overflow the model. Explicit
		// token_budget always wins.
		ccfg.TokenBudget = model.ContextLength * 8 / 10
	}

	out, stats := compress.Compress(*body, ccfg)
	if !stats.Applied {
		return &stats
	}
	*body = out
	if h.compressStore != nil {
		for _, m := range stats.Markers {
			h.compressStore.Put(m.Content)
		}
	}
	h.logger.Debug("input compression applied", logging.F(
		"model", internalModel,
		"tokens_before", stats.TokensBefore,
		"tokens_after", stats.TokensAfter,
		"rows_offloaded", stats.RowsOffloaded))
	return &stats
}

func modelCompression(m *store.Model) *config.CompressionConfig {
	if m == nil {
		return nil
	}
	return m.Compression
}

func keyCompression(k *store.APIKey) *config.CompressionConfig {
	if k == nil {
		return nil
	}
	return k.Compression
}

// Retrieve implements GET /v1/retrieve/{hash}, returning the original bytes for
// a row-drop retrieval marker (or 404 if absent/expired).
func (h *Handler) Retrieve(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if h.compressStore == nil || hash == "" {
		writeRetrieveError(w, http.StatusNotFound, "not_found")
		return
	}
	b, ok := h.compressStore.Get(hash)
	if !ok {
		writeRetrieveError(w, http.StatusNotFound, "not_found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func writeRetrieveError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"retrieval hash not found or expired"}}`))
}
