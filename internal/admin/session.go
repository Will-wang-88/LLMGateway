package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/will-wang-88/llmgateway/internal/proxy"
)

// SessionManager mints and validates short-lived session tokens for
// dashboard users. Tokens are HMAC-signed with a process-local secret
// so a leaked password isn't required to be cached on the client.
type SessionManager struct {
	secret []byte

	mu     sync.RWMutex
	revoke map[string]time.Time // token-id -> revoked-at
}

func NewSessionManager() *SessionManager {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return &SessionManager{secret: b, revoke: make(map[string]time.Time)}
}

type sessionClaim struct {
	ID        string `json:"jti"`
	Username  string `json:"sub"`
	Role      string `json:"role"`
	ExpiresAt int64  `json:"exp"`
}

// Mint creates a token valid for ttl, returning its opaque base64
// representation (payload.sig).
func (m *SessionManager) Mint(username, role string, ttl time.Duration) string {
	id := randID(12)
	claim := sessionClaim{ID: id, Username: username, Role: role, ExpiresAt: time.Now().Add(ttl).Unix()}
	payload, _ := json.Marshal(claim)
	enc := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(enc))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return enc + "." + sig
}

// Verify parses a token and returns the claim if the signature is
// valid, the token is not revoked, and it has not expired.
func (m *SessionManager) Verify(token string) (*sessionClaim, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, false
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	var c sessionClaim
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, false
	}
	if time.Now().Unix() > c.ExpiresAt {
		return nil, false
	}
	m.mu.RLock()
	_, revoked := m.revoke[c.ID]
	m.mu.RUnlock()
	if revoked {
		return nil, false
	}
	return &c, true
}

// Revoke marks the given token id as no longer accepted (logout).
func (m *SessionManager) Revoke(id string) {
	m.mu.Lock()
	m.revoke[id] = time.Now()
	m.mu.Unlock()
}

func randID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// loginHandler accepts HTTP Basic auth, verifies the user, and returns
// a short-lived session token so the dashboard can avoid caching the
// raw password.
func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Admin.Enabled {
		proxy.WriteError(w, http.StatusForbidden, proxy.PermissionError("Admin API is disabled", "admin_disabled"))
		return
	}
	if s.sessions == nil {
		proxy.WriteError(w, http.StatusServiceUnavailable, proxy.InternalError("Session manager not initialized", "no_sessions"))
		return
	}
	u, p, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="admin"`)
		proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized("Provide HTTP Basic credentials", "invalid_admin_credentials"))
		return
	}
	if s.users == nil {
		proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized("User store not configured", "invalid_admin_credentials"))
		return
	}
	user, found := s.users.Get(u)
	if !found || user.PasswordHash == "" || !VerifyPassword(p, user.PasswordHash) {
		proxy.WriteError(w, http.StatusUnauthorized, proxy.Unauthorized("Invalid admin credentials", "invalid_admin_credentials"))
		return
	}
	ttl := 12 * time.Hour
	token := s.sessions.Mint(user.Username, string(user.Role), ttl)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_in": int(ttl.Seconds()),
		"username":   user.Username,
		"role":       string(user.Role),
	})
}

// logoutHandler revokes the current bearer session token (if any).
func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if c, ok := s.sessions.Verify(token); ok {
			s.sessions.Revoke(c.ID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
