package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/will-wang-88/llmgateway/internal/config"
)

// Role enumerates admin roles.
type Role string

const (
	RoleSuperAdmin Role = "super_admin"
	RoleAdmin      Role = "admin"
	RoleOperator   Role = "operator"
	RoleViewer     Role = "viewer"
	RoleAuditor    Role = "auditor"
)

// User is a dashboard / admin-API user.
type User struct {
	Username     string
	Email        string
	PasswordHash string
	Role         Role
}

// Users is an in-memory user store.
type Users struct {
	mu      sync.RWMutex
	byName  map[string]*User
}

func NewUsers() *Users {
	return &Users{byName: make(map[string]*User)}
}

func (u *Users) LoadFromConfig(cfgs []config.AdminUserConfig) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range cfgs {
		role := Role(c.Role)
		if role == "" {
			role = RoleViewer
		}
		hash := c.PasswordHash
		if hash == "" && c.Password != "" {
			hash = HashPassword(c.Password)
		}
		u.byName[c.Username] = &User{
			Username:     c.Username,
			Email:        c.Email,
			PasswordHash: hash,
			Role:         role,
		}
	}
}

func (u *Users) Get(username string) (*User, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	x, ok := u.byName[username]
	return x, ok
}

func (u *Users) List() []*User {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]*User, 0, len(u.byName))
	for _, v := range u.byName {
		out = append(out, v)
	}
	return out
}

func (u *Users) Upsert(user *User) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.byName[user.Username] = user
}

func (u *Users) Delete(username string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.byName, username)
}

// HashPassword returns a bcrypt hash of the password (cost 12). The result
// is the canonical bcrypt `$2a$...` string; verification uses VerifyPassword.
func HashPassword(password string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		// bcrypt only fails on cost out-of-range; with a hardcoded valid
		// cost this can't happen.
		panic(err)
	}
	return string(h)
}

// VerifyPassword returns true if password matches the stored hash. Supports
// bcrypt hashes (the new format) and the legacy raw SHA-256 hex format
// from older configs, so existing deployments don't immediately lose login.
func VerifyPassword(password, stored string) bool {
	if strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") || strings.HasPrefix(stored, "$2y$") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) == nil
	}
	// legacy sha256(hex) - kept for backwards compatibility, deprecated.
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:]) == stored
}

// HasPermission returns whether the role can perform an action.
// Actions are short strings: "read", "manage_backends", "manage_keys",
// "manage_models", "view_logs", "view_audit", "manage_users".
func HasPermission(r Role, action string) bool {
	switch r {
	case RoleSuperAdmin:
		return true
	case RoleAdmin:
		return action != "manage_users"
	case RoleOperator:
		switch action {
		case "read", "backend_toggle", "view_logs":
			return true
		}
		return false
	case RoleViewer:
		return action == "read" || action == "view_logs"
	case RoleAuditor:
		return action == "read" || action == "view_logs" || action == "view_audit"
	}
	return false
}

// NormalizeRole maps casing-insensitive role strings to canonical form.
func NormalizeRole(s string) Role {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "super_admin", "superadmin":
		return RoleSuperAdmin
	case "admin":
		return RoleAdmin
	case "operator":
		return RoleOperator
	case "viewer":
		return RoleViewer
	case "auditor":
		return RoleAuditor
	}
	return ""
}
