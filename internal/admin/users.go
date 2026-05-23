package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

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

// HashPassword returns the SHA256 hex digest of a password. Not BCrypt - for an
// MVP without external crypto dependencies. Operators are expected to rotate
// credentials regularly and protect the YAML / DB at rest.
func HashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
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
