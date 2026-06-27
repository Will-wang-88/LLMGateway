package logstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Postgres implements Store on top of any database/sql driver that
// speaks PostgreSQL ($1, $2, ... placeholders). The driver is provided
// by the caller (lib/pq, pgx, etc.) via the *sql.DB.
type Postgres struct {
	db *sql.DB
}

// OpenPostgres takes a driver-registered DB. The caller is responsible
// for choosing the driver (e.g. lib/pq or pgx/stdlib) and pre-opening
// the connection; this keeps the gateway binary free of a hard pq
// dependency. The schema is created on first use.
func OpenPostgres(db *sql.DB) (*Postgres, error) {
	if db == nil {
		return nil, errors.New("postgres: nil *sql.DB")
	}
	p := &Postgres{db: db}
	if err := p.migrate(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Postgres) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL,
			api_key_id TEXT,
			api_key_name TEXT,
			client_ip TEXT,
			model TEXT NOT NULL,
			internal_model TEXT,
			backend_id TEXT,
			endpoint TEXT NOT NULL,
			stream BOOLEAN NOT NULL DEFAULT FALSE,
			status_code INTEGER,
			error_code TEXT,
			prompt_tokens BIGINT NOT NULL DEFAULT 0,
			completion_tokens BIGINT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			reasoning_tokens BIGINT NOT NULL DEFAULT 0,
			latency_ms BIGINT NOT NULL DEFAULT 0,
			ttft_ms BIGINT NOT NULL DEFAULT 0,
			raw_request TEXT,
			raw_response TEXT,
			metadata JSONB,
			created_at BIGINT NOT NULL,
			compression_applied BOOLEAN NOT NULL DEFAULT FALSE,
			original_tokens BIGINT NOT NULL DEFAULT 0,
			compressed_tokens BIGINT NOT NULL DEFAULT 0,
			compression_ratio DOUBLE PRECISION NOT NULL DEFAULT 0
		)`,
		// In-place upgrades for databases created before these columns existed.
		`ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS compression_applied BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS original_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS compressed_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS compression_ratio DOUBLE PRECISION NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_api_key ON request_logs(api_key_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_client_ip ON request_logs(client_ip, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_backend ON request_logs(backend_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_request ON request_logs(request_id)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id TEXT PRIMARY KEY,
			admin_user TEXT NOT NULL,
			action TEXT NOT NULL,
			target_type TEXT NOT NULL,
			target_id TEXT,
			old_value JSONB,
			new_value JSONB,
			ip TEXT,
			user_agent TEXT,
			created_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_user ON audit_logs(admin_user, created_at)`,
	}
	for _, q := range stmts {
		if _, err := p.db.Exec(q); err != nil {
			return fmt.Errorf("postgres migrate: %w", err)
		}
	}
	return nil
}

func (p *Postgres) Close() error { return p.db.Close() }

func (p *Postgres) AppendRequest(ctx context.Context, r *RequestLog) error {
	meta, _ := json.Marshal(r.Metadata)
	_, err := p.db.ExecContext(ctx, `INSERT INTO request_logs
		(id, request_id, api_key_id, api_key_name, client_ip, model, internal_model, backend_id,
		 endpoint, stream, status_code, error_code, prompt_tokens, completion_tokens,
		 total_tokens, reasoning_tokens, latency_ms, ttft_ms, raw_request, raw_response,
		 metadata, created_at, compression_applied, original_tokens, compressed_tokens, compression_ratio)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		r.ID, r.RequestID, nullable(r.APIKeyID), nullable(r.APIKeyName), nullable(r.ClientIP),
		r.Model, nullable(r.InternalModel), nullable(r.BackendID), r.Endpoint, r.Stream,
		r.StatusCode, nullable(r.ErrorCode), r.PromptTokens, r.CompletionTokens,
		r.TotalTokens, r.ReasoningTokens, r.LatencyMS, r.TTFTMS,
		nullable(r.RawRequest), nullable(r.RawResponse), string(meta),
		r.CreatedAt.UTC().UnixMilli(), r.CompressionApplied, r.OriginalTokens,
		r.CompressedTokens, r.CompressionRatio)
	return err
}

func (p *Postgres) QueryRequests(ctx context.Context, q LogQuery) ([]*RequestLog, error) {
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	conds := []string{}
	args := []any{}
	add := func(col string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if q.RequestID != "" {
		add("request_id", q.RequestID)
	}
	if q.APIKeyID != "" {
		add("api_key_id", q.APIKeyID)
	}
	if q.ClientIP != "" {
		add("client_ip", q.ClientIP)
	}
	if q.Model != "" {
		add("model", q.Model)
	}
	if q.BackendID != "" {
		add("backend_id", q.BackendID)
	}
	if q.Endpoint != "" {
		add("endpoint", q.Endpoint)
	}
	if q.StatusCode != 0 {
		add("status_code", q.StatusCode)
	}
	if q.ErrorCode != "" {
		add("error_code", q.ErrorCode)
	}
	if q.Stream != nil {
		add("stream", *q.Stream)
	}
	if q.Since != nil {
		args = append(args, q.Since.UTC().UnixMilli())
		conds = append(conds, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if q.Until != nil {
		args = append(args, q.Until.UTC().UnixMilli())
		conds = append(conds, fmt.Sprintf("created_at <= $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, q.Offset)
	sqlStr := fmt.Sprintf(`SELECT id, request_id, COALESCE(api_key_id,''), COALESCE(api_key_name,''),
		COALESCE(client_ip,''), model, COALESCE(internal_model,''), COALESCE(backend_id,''),
		endpoint, stream, status_code, COALESCE(error_code,''), prompt_tokens, completion_tokens,
		total_tokens, reasoning_tokens, latency_ms, ttft_ms, COALESCE(raw_request,''),
		COALESCE(raw_response,''), COALESCE(metadata::text,''), created_at,
		COALESCE(compression_applied,FALSE), COALESCE(original_tokens,0),
		COALESCE(compressed_tokens,0), COALESCE(compression_ratio,0)
		FROM request_logs %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		where, len(args)-1, len(args))
	rows, err := p.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*RequestLog, 0, limit)
	for rows.Next() {
		r := &RequestLog{}
		var createdAt int64
		var metadata string
		if err := rows.Scan(&r.ID, &r.RequestID, &r.APIKeyID, &r.APIKeyName, &r.ClientIP,
			&r.Model, &r.InternalModel, &r.BackendID, &r.Endpoint, &r.Stream, &r.StatusCode,
			&r.ErrorCode, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.ReasoningTokens, &r.LatencyMS, &r.TTFTMS, &r.RawRequest, &r.RawResponse,
			&metadata, &createdAt, &r.CompressionApplied, &r.OriginalTokens,
			&r.CompressedTokens, &r.CompressionRatio); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(createdAt).UTC()
		if metadata != "" {
			_ = json.Unmarshal([]byte(metadata), &r.Metadata)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) StatsSince(ctx context.Context, since time.Time) (*Stats, error) {
	sinceMS := since.UTC().UnixMilli()
	stats := &Stats{
		ByModel:    make(map[string]ModelStat),
		ByBackend:  make(map[string]BackendStat),
		ByAPIKey:   make(map[string]KeyStat),
		ByClientIP: make(map[string]ClientIPStat),
	}
	row := p.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 400 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(CASE WHEN compression_applied THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN compression_applied THEN original_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN compression_applied THEN compressed_tokens ELSE 0 END),0)
		FROM request_logs WHERE created_at >= $1`, sinceMS)
	if err := row.Scan(&stats.TotalRequests, &stats.SuccessTotal, &stats.ErrorTotal,
		&stats.PromptTokens, &stats.CompletionTokens, &stats.TotalTokens,
		&stats.CompressedRequests, &stats.OriginalTokens, &stats.CompressedTokens); err != nil {
		return nil, err
	}
	groupQ := func(q string, fn func(string, int64, int64, int64)) error {
		rows, err := p.db.QueryContext(ctx, q, sinceMS)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var reqs, errs, toks int64
			if err := rows.Scan(&k, &reqs, &errs, &toks); err != nil {
				return err
			}
			fn(k, reqs, errs, toks)
		}
		return rows.Err()
	}
	if err := groupQ(`SELECT model, COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= $1 GROUP BY model`,
		func(k string, r, e, t int64) {
			stats.ByModel[k] = ModelStat{Requests: r, Errors: e, Tokens: t}
		}); err != nil {
		return nil, err
	}
	if err := groupQ(`SELECT COALESCE(backend_id,''), COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= $1 GROUP BY backend_id`,
		func(k string, r, e, t int64) {
			stats.ByBackend[k] = BackendStat{Requests: r, Errors: e, Tokens: t}
		}); err != nil {
		return nil, err
	}
	if err := groupQ(`SELECT COALESCE(api_key_id,''), COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= $1 GROUP BY api_key_id`,
		func(k string, r, e, t int64) {
			stats.ByAPIKey[k] = KeyStat{Requests: r, Errors: e, Tokens: t}
		}); err != nil {
		return nil, err
	}
	if err := groupQ(`SELECT COALESCE(client_ip,''), COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(total_tokens),0)
		FROM request_logs WHERE created_at >= $1 AND client_ip IS NOT NULL AND client_ip <> ''
		GROUP BY client_ip`,
		func(k string, r, e, t int64) {
			stats.ByClientIP[k] = ClientIPStat{Requests: r, Errors: e, Tokens: t}
		}); err != nil {
		return nil, err
	}
	finalizeCompressionStats(stats)
	return stats, nil
}

func (p *Postgres) AppendAudit(ctx context.Context, e *AuditEvent) error {
	var oldJSON, newJSON []byte
	if e.OldValue != nil {
		oldJSON, _ = json.Marshal(e.OldValue)
	}
	if e.NewValue != nil {
		newJSON, _ = json.Marshal(e.NewValue)
	}
	_, err := p.db.ExecContext(ctx, `INSERT INTO audit_logs
		(id, admin_user, action, target_type, target_id, old_value, new_value, ip, user_agent, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.ID, e.AdminUser, e.Action, e.TargetType, nullable(e.TargetID),
		nullable(string(oldJSON)), nullable(string(newJSON)),
		nullable(e.IP), nullable(e.UserAgent), e.CreatedAt.UTC().UnixMilli())
	return err
}

func (p *Postgres) QueryAudit(ctx context.Context, q AuditQuery) ([]*AuditEvent, error) {
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	conds := []string{}
	args := []any{}
	add := func(col string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if q.AdminUser != "" {
		add("admin_user", q.AdminUser)
	}
	if q.Action != "" {
		add("action", q.Action)
	}
	if q.TargetType != "" {
		add("target_type", q.TargetType)
	}
	if q.TargetID != "" {
		add("target_id", q.TargetID)
	}
	if q.Since != nil {
		args = append(args, q.Since.UTC().UnixMilli())
		conds = append(conds, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if q.Until != nil {
		args = append(args, q.Until.UTC().UnixMilli())
		conds = append(conds, fmt.Sprintf("created_at <= $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, q.Offset)
	sqlStr := fmt.Sprintf(`SELECT id, admin_user, action, target_type, COALESCE(target_id,''),
		COALESCE(old_value::text,''), COALESCE(new_value::text,''), COALESCE(ip,''),
		COALESCE(user_agent,''), created_at FROM audit_logs %s
		ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))
	rows, err := p.db.QueryContext(ctx, sqlStr, args...)
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

// Purge deletes records older than the retention window. Mirrors the
// SQLite driver for symmetry.
func (p *Postgres) Purge(ctx context.Context, retention time.Duration) error {
	cutoff := time.Now().Add(-retention).UTC().UnixMilli()
	if _, err := p.db.ExecContext(ctx, `DELETE FROM request_logs WHERE created_at < $1`, cutoff); err != nil {
		return err
	}
	_, err := p.db.ExecContext(ctx, `DELETE FROM audit_logs WHERE created_at < $1`, cutoff)
	return err
}
