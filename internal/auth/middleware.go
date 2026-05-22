package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

type ctxKey struct{ name string }

var apiKeyCtxKey = ctxKey{"apiKey"}

type Authenticator struct {
	store    *store.Store
	header   string
	prefix   string
}

func New(s *store.Store, header, prefix string) *Authenticator {
	if header == "" {
		header = "Authorization"
	}
	if prefix == "" {
		prefix = "Bearer "
	}
	return &Authenticator{store: s, header: header, prefix: prefix}
}

// Middleware enforces a valid API key on requests that match the configured matcher.
// matcher returns true for paths that require auth.
func (a *Authenticator) Middleware(matcher func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if matcher != nil && !matcher(r) {
				next.ServeHTTP(w, r)
				return
			}
			raw := a.extractKey(r)
			if raw == "" {
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized(
					"Missing API key in "+a.header+" header",
					"invalid_api_key",
				))
				return
			}
			key, ok := a.store.APIKeyByRaw(raw)
			if !ok {
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized(
					"Invalid API key",
					"invalid_api_key",
				))
				return
			}
			if !key.Enabled {
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized(
					"API key is disabled",
					"invalid_api_key",
				))
				return
			}
			if !key.ExpiresAt.IsZero() && time.Now().After(key.ExpiresAt) {
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized(
					"API key has expired",
					"invalid_api_key",
				))
				return
			}
			ctx := context.WithValue(r.Context(), apiKeyCtxKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (a *Authenticator) extractKey(r *http.Request) string {
	raw := r.Header.Get(a.header)
	if raw == "" {
		return ""
	}
	if a.prefix != "" && strings.HasPrefix(raw, a.prefix) {
		return strings.TrimSpace(strings.TrimPrefix(raw, a.prefix))
	}
	return strings.TrimSpace(raw)
}

func FromContext(ctx context.Context) (*store.APIKey, bool) {
	v := ctx.Value(apiKeyCtxKey)
	if v == nil {
		return nil, false
	}
	k, ok := v.(*store.APIKey)
	return k, ok
}
