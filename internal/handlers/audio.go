package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/will-wang-88/llmgateway/internal/auth"
	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/proxy"
)

// ForwardMultipart handles /v1/audio/transcriptions and similar multipart
// endpoints. The model field is read from the multipart form, then the body
// is re-emitted (field-by-field) to the chosen backend.
func (h *Handler) ForwardMultipart(upstreamPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.New().String()
		w.Header().Set("X-Request-ID", requestID)
		started := time.Now()

		apiKey, _ := auth.FromContext(r.Context())

		contentType := r.Header.Get("Content-Type")
		if !strings.HasPrefix(contentType, "multipart/form-data") {
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Expected multipart/form-data Content-Type",
				"invalid_content_type",
			))
			return
		}

		limit := int64(h.cfg.Server.RequestBodyLimitMB) * 1024 * 1024
		if limit <= 0 {
			limit = 50 * 1024 * 1024
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if err := r.ParseMultipartForm(32 * 1024 * 1024); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				proxy.WriteError(w, http.StatusRequestEntityTooLarge, proxy.APIError{
					Message: "Request body too large",
					Type:    "invalid_request_error",
					Code:    "payload_too_large",
				})
				return
			}
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Invalid multipart form: "+err.Error(),
				"invalid_multipart",
			))
			return
		}
		model := r.FormValue("model")
		if model == "" {
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Missing required field: model",
				"missing_model",
			))
			return
		}

		internalModel, _ := h.store.ResolveAlias(model)

		if apiKey != nil && !apiKey.ModelAllowedResolved(model, internalModel) {
			proxy.WriteError(w, http.StatusForbidden, proxy.PermissionError(
				fmt.Sprintf("The API key is not allowed to use model: %s", model),
				"model_not_allowed",
			))
			return
		}

		// Honor model registry enabled=false as a routing kill switch.
		if m, ok := h.store.Model(internalModel); ok && !m.Enabled {
			proxy.WriteError(w, http.StatusNotFound, proxy.NotFound(
				fmt.Sprintf("Model is disabled: %s", model),
				"model_not_found",
			))
			return
		}

		candidates := h.store.BackendsForModel(internalModel)
		if len(candidates) == 0 {
			proxy.WriteError(w, http.StatusNotFound, proxy.NotFound(
				fmt.Sprintf("Unknown model: %s", model),
				"model_not_found",
			))
			return
		}

		// Apply rate-limit / quota / concurrency / queue admission.
		releaseAdmit, code, status := h.admit(r.Context(), apiKey, internalModel)
		if code != "" {
			proxy.WriteError(w, status, proxy.RateLimit("Rejected: "+code, code))
			return
		}
		defer releaseAdmit()

		ready := filterRoutable(candidates, h.cfg.Routing.AllowDegradedBackends)
		if len(ready) == 0 {
			proxy.WriteError(w, http.StatusServiceUnavailable, proxy.BackendUnavailable(
				fmt.Sprintf("No healthy backend available for model: %s", model),
				"no_healthy_backend",
			))
			return
		}
		policy := balancer.Policy(h.cfg.Routing.DefaultPolicy)
		hint := balancer.Hint{APIKeyID: apiKeyID(apiKey)}
		picked := h.balancer.Choose(internalModel, policy, ready, hint)
		if picked == nil || !picked.AcquireSlot() {
			proxy.WriteError(w, http.StatusServiceUnavailable, proxy.BackendUnavailable(
				"All matching backends at capacity", "backend_at_capacity",
			))
			return
		}

		bodyBuf, ct, err := rebuildMultipart(r)
		// Always release multipart temp files, even on error.
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		if err != nil {
			picked.ReleaseSlot(false)
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Failed to rebuild multipart body: "+err.Error(), "invalid_multipart",
			))
			return
		}

		apiKeyLabel := apiKeyID(apiKey)
		h.metrics.ActiveRequests.WithLabelValues(internalModel, picked.ID).Inc()
		defer h.metrics.ActiveRequests.WithLabelValues(internalModel, picked.ID).Dec()

		opts := proxy.ForwardOptions{
			Method:       http.MethodPost,
			Backend:      picked,
			UpstreamPath: upstreamPath,
			Body:         bodyBuf,
			IsStream:     false,
			TimeoutMS:    picked.TimeoutMS,
			ContentType:  ct,
			Model:        internalModel,
			ForwardModel: model,
			APIKeyLabel:  apiKeyLabel,
			Endpoint:     upstreamPath,
		}
		statusCode, ferr := h.proxy.Forward(r.Context(), w, r, opts)
		success := ferr == nil && statusCode >= 200 && statusCode < 400
		picked.ReleaseSlot(success)
		if !success {
			h.metrics.BackendErrors.WithLabelValues(picked.ID, statusCodeLabel(statusCode)).Inc()
		}
		policyLabel := h.cfg.Routing.DefaultPolicy
		if policyLabel == "" {
			policyLabel = "unknown"
		}
		h.metrics.Requests.WithLabelValues(
			upstreamPath, internalModel, picked.ID, apiKeyLabel, statusCodeLabel(statusCode), "false", policyLabel,
		).Inc()
		h.metrics.RequestLatency.WithLabelValues(
			upstreamPath, internalModel, picked.ID, "false", policyLabel,
		).Observe(time.Since(started).Seconds())

		latencyMS := time.Since(started).Milliseconds()
		if apiKey != nil {
			apiKey.TouchRequest()
		}
		h.recordLog(r.Context(), requestID, apiKey, auth.ClientIPFromContext(r.Context()), model, internalModel, picked.ID,
			upstreamPath, false, statusCode, errorCodeFromForward(ferr, statusCode),
			nil, latencyMS, 0, nil, nil, nil)
	}
}

// rebuildMultipart re-emits the parsed multipart form (fields + files) into a
// new body suitable for forwarding to the backend. Returns the encoded body
// and the new Content-Type (with boundary).
func rebuildMultipart(r *http.Request) ([]byte, string, error) {
	if r.MultipartForm == nil {
		return nil, "", errors.New("multipart not parsed")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for key, vals := range r.MultipartForm.Value {
		for _, v := range vals {
			if err := mw.WriteField(key, v); err != nil {
				return nil, "", err
			}
		}
	}
	for key, headers := range r.MultipartForm.File {
		for _, fh := range headers {
			f, err := fh.Open()
			if err != nil {
				return nil, "", err
			}
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, fh.Filename))
			if ct := fh.Header.Get("Content-Type"); ct != "" {
				h.Set("Content-Type", ct)
			}
			part, err := mw.CreatePart(h)
			if err != nil {
				_ = f.Close()
				return nil, "", err
			}
			if _, err := io.Copy(part, f); err != nil {
				_ = f.Close()
				return nil, "", err
			}
			_ = f.Close()
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
