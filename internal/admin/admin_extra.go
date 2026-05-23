package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/proxy"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, _ := AdminUserFromContext(r.Context())
	if user == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username":      user.Username,
		"email":         user.Email,
		"role":          string(user.Role),
		"authenticated": true,
	})
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("view_logs", w, r) {
		return
	}
	if s.logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	q := logstore.LogQuery{
		RequestID: r.URL.Query().Get("request_id"),
		APIKeyID:  r.URL.Query().Get("api_key_id"),
		Model:     r.URL.Query().Get("model"),
		BackendID: r.URL.Query().Get("backend_id"),
		Endpoint:  r.URL.Query().Get("endpoint"),
		ErrorCode: r.URL.Query().Get("error_code"),
	}
	if v := r.URL.Query().Get("status_code"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			q.StatusCode = i
		}
	}
	if v := r.URL.Query().Get("stream"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		q.Stream = &b
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Since = &t
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Until = &t
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			q.Limit = i
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			q.Offset = i
		}
	}
	rows, err := s.logs.QueryRequests(r.Context(), q)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, proxy.InternalError("query logs: "+err.Error(), "query_logs_failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": rows, "count": len(rows)})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("view_audit", w, r) {
		return
	}
	if s.logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	q := logstore.AuditQuery{
		AdminUser:  r.URL.Query().Get("admin_user"),
		Action:     r.URL.Query().Get("action"),
		TargetType: r.URL.Query().Get("target_type"),
		TargetID:   r.URL.Query().Get("target_id"),
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			q.Limit = i
		}
	}
	rows, err := s.logs.QueryAudit(r.Context(), q)
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, proxy.InternalError("query audit: "+err.Error(), "query_audit_failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": rows, "count": len(rows)})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_users", w, r) {
		return
	}
	if s.users == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	type entry struct {
		Username string `json:"username"`
		Email    string `json:"email,omitempty"`
		Role     string `json:"role"`
	}
	out := make([]entry, 0)
	for _, u := range s.users.List() {
		out = append(out, entry{Username: u.Username, Email: u.Email, Role: string(u.Role)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

type userBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_users", w, r) {
		return
	}
	var b userBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Invalid JSON: "+err.Error(), "invalid_json"))
		return
	}
	if b.Username == "" || b.Password == "" {
		proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("username and password required", "missing_fields"))
		return
	}
	role := NormalizeRole(b.Role)
	if role == "" {
		role = RoleViewer
	}
	u := &User{Username: b.Username, Email: b.Email, Role: role, PasswordHash: HashPassword(b.Password)}
	s.users.Upsert(u)
	s.audit(r, "user.create", "user", u.Username, nil, map[string]any{"username": u.Username, "role": string(role)})
	writeJSON(w, http.StatusCreated, map[string]any{"username": u.Username, "role": string(role)})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requirePerm("manage_users", w, r) {
		return
	}
	username := r.PathValue("username")
	s.users.Delete(username)
	s.audit(r, "user.delete", "user", username, nil, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) keyUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.quota == nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "usage": nil})
		return
	}
	dayR, dayT, monR, monT := s.quota.Usage(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id,
		"day_requests":     dayR,
		"day_tokens":       dayT,
		"month_requests":   monR,
		"month_tokens":     monT,
	})
}
