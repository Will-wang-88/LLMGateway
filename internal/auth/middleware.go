package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/will-wang-88/llmgateway/internal/logstore"
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
	logstore  logstore.Store
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

// WithLogStore wires a persistent log store so requests rejected at the
// auth layer (missing / invalid / disabled / expired key, denied client
// IP) still appear in the gateway's audit trail with their client_ip.
func (a *Authenticator) WithLogStore(ls logstore.Store) *Authenticator {
	a.logstore = ls
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
			reject := func(status int, code, msg string, k *store.APIKey, asPermission bool) {
				a.logReject(ctx, r, k, clientIP, status, code)
				if asPermission {
					proxy.WriteError(w, status, proxy.PermissionError(msg, code))
				} else {
					proxy.WriteError(w, status, proxy.Unauthorized(msg, code))
				}
			}
			raw, prefixOK := a.extractKey(r)
			if !prefixOK {
				reject(http.StatusUnauthorized, "invalid_api_key",
					"Authorization header must use the configured prefix", nil, false)
				return
			}
			if raw == "" {
				reject(http.StatusUnauthorized, "invalid_api_key",
					"Missing API key in "+a.header+" header", nil, false)
				return
			}
			key, ok := a.store.APIKeyByRaw(raw)
			if !ok {
				reject(http.StatusUnauthorized, "invalid_api_key", "Invalid API key", nil, false)
				return
			}
			if !key.Enabled {
				reject(http.StatusUnauthorized, "invalid_api_key", "API key is disabled", key, false)
				return
			}
			if !key.ExpiresAt.IsZero() && time.Now().After(key.ExpiresAt) {
				reject(http.StatusUnauthorized, "invalid_api_key", "API key has expired", key, false)
				return
			}
			// Per-key client IP allow / deny. Deny takes precedence over
			// allow. An empty allowed list means "no IP restriction".
			if len(key.DeniedClientIPs) > 0 && netutil.MatchAny(clientIP, key.DeniedClientIPs) {
				reject(http.StatusForbidden, "client_ip_not_allowed",
					"Client IP is not allowed for this API key", key, true)
				return
			}
			if len(key.AllowedClientIPs) > 0 && !netutil.MatchAny(clientIP, key.AllowedClientIPs) {
				reject(http.StatusForbidden, "client_ip_not_allowed",
					"Client IP is not allowed for this API key", key, true)
				return
			}
			ctx = context.WithValue(ctx, apiKeyCtxKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// logReject writes a persistent request_log entry for an auth-layer
// rejection. Without this, the only trace of a missing / invalid /
// denied-IP attempt would be a structured stderr log line that never
// makes it to the audit trail.
func (a *Authenticator) logReject(ctx context.Context, r *http.Request, k *store.APIKey, clientIP string, status int, code string) {
	if a.logstore == nil {
		return
	}
	rec := &logstore.RequestLog{
		ID:         uuid.New().String(),
		RequestID:  uuid.New().String(),
		ClientIP:   clientIP,
		Endpoint:   r.URL.Path,
		StatusCode: status,
		ErrorCode:  code,
		CreatedAt:  time.Now().UTC(),
	}
	if k != nil {
		rec.APIKeyID = k.ID
		rec.APIKeyName = k.Name
	}
	go func(rec *logstore.RequestLog) {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.logstore.AppendRequest(c, rec)
	}(rec)
	_ = ctx
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
