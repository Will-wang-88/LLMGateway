package logstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLite implements Store on top of a SQLite database. Uses modernc.org/sqlite
// (pure-Go, no CGO required).
type SQLite struct {
	db *sql.DB
}

func OpenSQLite(path string) (*SQLite, error) {
	if path == "" {
		path = "llmgateway.db"
	}
	// Build a DSN that enables WAL + reasonable concurrency.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; WAL allows concurrent reads.
	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL,
			api_key_id TEXT,
			api_key_name TEXT,
			model TEXT NOT NULL,
			internal_model TEXT,
			backend_id TEXT,
			endpoint TEXT NOT NULL,
			stream INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER,
			error_code TEXT,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			ttft_ms INTEGER NOT NULL DEFAULT 0,
			raw_request TEXT,
			raw_response TEXT,
			metadata TEXT,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_api_key ON request_logs(api_key_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_backend ON request_logs(backend_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_request ON request_logs(request_id)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id TEXT PRIMARY KEY,
			admin_user TEXT NOT NULL,
			action TEXT NOT NULL,
			target_type TEXT NOT NULL,
			target_id TEXT,
			old_value TEXT,
			new_value TEXT,
			ip TEXT,
			user_agent TEXT,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_user ON audit_logs(admin_user, created_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) AppendRequest(ctx context.Context, r *RequestLog) error {
	meta, _ := json.Marshal(r.Metadata)
	_, err := s.db.ExecContext(ctx, `INSERT INTO request_logs
		(id, request_id, api_key_id, api_key_name, model, internal_model, backend_id,
		 endpoint, stream, status_code, error_code, prompt_tokens, completion_tokens,
		 total_tokens, reasoning_tokens, latency_ms, ttft_ms, raw_request, raw_response,
		 metadata, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.RequestID, nullable(r.APIKeyID), nullable(r.APIKeyName), r.Model,
		nullable(r.InternalModel), nullable(r.BackendID), r.Endpoint, boolInt(r.Stream),
		r.StatusCode, nullable(r.ErrorCode), r.PromptTokens, r.CompletionTokens,
		r.TotalTokens, r.ReasoningTokens, r.LatencyMS, r.TTFTMS,
		nullable(r.RawRequest), nullable(r.RawResponse), string(meta),
		r.CreatedAt.UTC().UnixMilli())
	return err
}

func (s *SQLite) QueryRequests(ctx context.Context, q LogQuery) ([]*RequestLog, error) {
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	conds := []string{}
	args := []any{}
	if q.RequestID != "" {
		conds = append(conds, "request_id = ?")
		args = append(args, q.RequestID)
	}
	if q.APIKeyID != "" {
		conds = append(conds, "api_key_id = ?")
		args = append(args, q.APIKeyID)
	}
	if q.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, q.Model)
	}
	if q.BackendID != "" {
		conds = append(conds, "backend_id = ?")
		args = append(args, q.BackendID)
	}
	if q.Endpoint != "" {
		conds = append(conds, "endpoint = ?")
		args = append(args, q.Endpoint)
	}
	if q.StatusCode != 0 {
		conds = append(conds, "status_code = ?")
		args = append(args, q.StatusCode)
	}
	if q.ErrorCode != "" {
		conds = append(conds, "error_code = ?")
		args = append(args, q.ErrorCode)
	}
	if q.Stream != nil {
		conds = append(conds, "stream = ?")
		args = append(args, boolInt(*q.Stream))
	}
	if q.Since != nil {
		conds = append(conds, "created_at >= ?")
		args = append(args, q.Since.UTC().UnixMilli())
	}
	if q.Until != nil {
		conds = append(conds, "created_at <= ?")
		args = append(args, q.Until.UTC().UnixMilli())
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	sqlStr := fmt.Sprintf(`SELECT id, request_id, COALESCE(api_key_id,''), COALESCE(api_key_name,''),
		model, COALESCE(internal_model,''), COALESCE(backend_id,''), endpoint, stream,
		status_code, COALESCE(error_code,''), prompt_tokens, completion_tokens, total_tokens,
		reasoning_tokens, latency_ms, ttft_ms, COALESCE(raw_request,''), COALESCE(raw_response,''),
		COALESCE(metadata,''), created_at
		FROM request_logs %s ORDER BY created_at DESC LIMIT ? OFFSET ?`, where)
	args = append(args, limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*RequestLog, 0, limit)
	for rows.Next() {
		r := &RequestLog{}
		var createdAt int64
		var streamInt int
		var metadata string
		if err := rows.Scan(&r.ID, &r.RequestID, &r.APIKeyID, &r.APIKeyName, &r.Model,
			&r.InternalModel, &r.BackendID, &r.Endpoint, &streamInt, &r.StatusCode,
			&r.ErrorCode, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.ReasoningTokens, &r.LatencyMS, &r.TTFTMS, &r.RawRequest, &r.RawResponse,
			&metadata, &createdAt); err != nil {
			return nil, err
		}
		r.Stream = streamInt != 0
		r.CreatedAt = time.UnixMilli(createdAt).UTC()
		if metadata != "" {
			_ = json.Unmarshal([]byte(metadata), &r.Metadata)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLite) StatsSince(ctx context.Context, since time.Time) (*Stats, error) {
	sinceMS := since.UTC().UnixMilli()
	stats := &Stats{
		ByModel:   make(map[string]ModelStat),
		ByBackend: make(map[string]BackendStat),
		ByAPIKey:  make(map[string]KeyStat),
	}

	row := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		SUM(CASE WHEN status_code >= 200 AND status_code < 400 THEN 1 ELSE 0 END),
		SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= ?`, sinceMS)
	if err := row.Scan(&stats.TotalRequests, &stats.SuccessTotal, &stats.ErrorTotal,
		&stats.PromptTokens, &stats.CompletionTokens, &stats.TotalTokens); err != nil {
		return nil, err
	}

	if err := s.aggregate(ctx, `SELECT model,
		COUNT(*),
		SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= ? GROUP BY model`, sinceMS,
		func(key string, reqs, errs, tokens int64) {
			stats.ByModel[key] = ModelStat{Requests: reqs, Errors: errs, Tokens: tokens}
		}); err != nil {
		return nil, err
	}
	if err := s.aggregate(ctx, `SELECT COALESCE(backend_id,''),
		COUNT(*),
		SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= ? GROUP BY backend_id`, sinceMS,
		func(key string, reqs, errs, tokens int64) {
			stats.ByBackend[key] = BackendStat{Requests: reqs, Errors: errs, Tokens: tokens}
		}); err != nil {
		return nil, err
	}
	if err := s.aggregate(ctx, `SELECT COALESCE(api_key_id,''),
		COUNT(*),
		SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= ? GROUP BY api_key_id`, sinceMS,
		func(key string, reqs, errs, tokens int64) {
			stats.ByAPIKey[key] = KeyStat{Requests: reqs, Errors: errs, Tokens: tokens}
		}); err != nil {
		return nil, err
	}
	return stats, nil
}

func (s *SQLite) aggregate(ctx context.Context, q string, since int64, fn func(string, int64, int64, int64)) error {
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var reqs, errs, tokens int64
		if err := rows.Scan(&key, &reqs, &errs, &tokens); err != nil {
			return err
		}
		fn(key, reqs, errs, tokens)
	}
	return rows.Err()
}

func (s *SQLite) AppendAudit(ctx context.Context, e *AuditEvent) error {
	var oldJSON, newJSON []byte
	if e.OldValue != nil {
		oldJSON, _ = json.Marshal(e.OldValue)
	}
	if e.NewValue != nil {
		newJSON, _ = json.Marshal(e.NewValue)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_logs
		(id, admin_user, action, target_type, target_id, old_value, new_value, ip, user_agent, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.AdminUser, e.Action, e.TargetType, nullable(e.TargetID),
		nullable(string(oldJSON)), nullable(string(newJSON)),
		nullable(e.IP), nullable(e.UserAgent), e.CreatedAt.UTC().UnixMilli())
	return err
}

func (s *SQLite) QueryAudit(ctx context.Context, q AuditQuery) ([]*AuditEvent, error) {
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	conds := []string{}
	args := []any{}
	if q.AdminUser != "" {
		conds = append(conds, "admin_user = ?")
		args = append(args, q.AdminUser)
	}
	if q.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, q.Action)
	}
	if q.TargetType != "" {
		conds = append(conds, "target_type = ?")
		args = append(args, q.TargetType)
	}
	if q.TargetID != "" {
		conds = append(conds, "target_id = ?")
		args = append(args, q.TargetID)
	}
	if q.Since != nil {
		conds = append(conds, "created_at >= ?")
		args = append(args, q.Since.UTC().UnixMilli())
	}
	if q.Until != nil {
		conds = append(conds, "created_at <= ?")
		args = append(args, q.Until.UTC().UnixMilli())
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, q.Offset)
	sqlStr := fmt.Sprintf(`SELECT id, admin_user, action, target_type, COALESCE(target_id,''),
		COALESCE(old_value,''), COALESCE(new_value,''), COALESCE(ip,''), COALESCE(user_agent,''),
		created_at FROM audit_logs %s ORDER BY created_at DESC LIMIT ? OFFSET ?`, where)
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*AuditEvent, 0, limit)
	for rows.Next() {
		var e AuditEvent
		var oldJSON, newJSON string
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.AdminUser, &e.Action, &e.TargetType, &e.TargetID,
			&oldJSON, &newJSON, &e.IP, &e.UserAgent, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = time.UnixMilli(createdAt).UTC()
		if oldJSON != "" {
			_ = json.Unmarshal([]byte(oldJSON), &e.OldValue)
		}
		if newJSON != "" {
			_ = json.Unmarshal([]byte(newJSON), &e.NewValue)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// Purge deletes records older than the retention window.
func (s *SQLite) Purge(ctx context.Context, retention time.Duration) error {
	cutoff := time.Now().Add(-retention).UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM audit_logs WHERE created_at < ?`, cutoff)
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
