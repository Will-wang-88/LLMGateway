package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/will-wang-88/llmgateway/internal/netutil"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

type ctxKey struct{ name string }

var (
	apiKeyCtxKey   = ctxKey{"apiKey"}
	clientIPCtxKey = ctxKey{"clientIP"}
)

type Authenticator struct {
	store     *store.Store
	header    string
	prefix    string
	extractor *netutil.Extractor
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

// WithClientIPExtractor wires a shared client-IP extractor so the same
// trusted-proxy policy is applied for auth, request logs and stats.
func (a *Authenticator) WithClientIPExtractor(e *netutil.Extractor) *Authenticator {
	a.extractor = e
	return a
}

// Middleware enforces a valid API key on requests that match the configured matcher.
// matcher returns true for paths that require auth.
func (a *Authenticator) Middleware(matcher func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			clientIP := ""
			if a.extractor != nil {
				clientIP = a.extractor.ClientIP(r)
				ctx = context.WithValue(ctx, clientIPCtxKey, clientIP)
			}
			if matcher != nil && !matcher(r) {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			raw, prefixOK := a.extractKey(r)
			if !prefixOK {
				proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized(
					"Authorization header must use the configured prefix",
					"invalid_api_key",
				))
				return
			}
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
			// Per-key client IP allow / deny. Deny takes precedence over
			// allow. An empty allowed list means "no IP restriction".
			if len(key.DeniedClientIPs) > 0 && netutil.MatchAny(clientIP, key.DeniedClientIPs) {
				proxy.WriteError(w, http.StatusForbidden, proxy.PermissionError(
					"Client IP is not allowed for this API key",
					"client_ip_not_allowed",
				))
				return
			}
			if len(key.AllowedClientIPs) > 0 && !netutil.MatchAny(clientIP, key.AllowedClientIPs) {
				proxy.WriteError(w, http.StatusForbidden, proxy.PermissionError(
					"Client IP is not allowed for this API key",
					"client_ip_not_allowed",
				))
				return
			}
			ctx = context.WithValue(ctx, apiKeyCtxKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractKey returns (rawKey, prefixOK). If the configured prefix is set
// and the header does not start with that prefix, prefixOK is false so
// the caller can reject the request rather than treating the whole
// header value as a raw key.
func (a *Authenticator) extractKey(r *http.Request) (string, bool) {
	raw := r.Header.Get(a.header)
	if raw == "" {
		return "", true
	}
	if a.prefix == "" {
		return strings.TrimSpace(raw), true
	}
	if !strings.HasPrefix(raw, a.prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(raw, a.prefix)), true
}

func FromContext(ctx context.Context) (*store.APIKey, bool) {
	v := ctx.Value(apiKeyCtxKey)
	if v == nil {
		return nil, false
	}
	k, ok := v.(*store.APIKey)
	return k, ok
}

// ClientIPFromContext returns the trusted client IP previously stamped
// onto the request context by the auth middleware. Empty string if the
// middleware did not run (e.g. health/admin paths) or no IP was resolved.
func ClientIPFromContext(ctx context.Context) string {
	v, _ := ctx.Value(clientIPCtxKey).(string)
	return v
}
