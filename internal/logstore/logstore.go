package logstore

import (
	"context"
	"sync"
	"time"
)

// RequestLog is a single proxy request record.
type RequestLog struct {
	ID               string            `json:"id"`
	RequestID        string            `json:"request_id"`
	APIKeyID         string            `json:"api_key_id"`
	APIKeyName       string            `json:"api_key_name,omitempty"`
	ClientIP         string            `json:"client_ip,omitempty"`
	Model            string            `json:"model"`
	InternalModel    string            `json:"internal_model,omitempty"`
	BackendID        string            `json:"backend_id"`
	Endpoint         string            `json:"endpoint"`
	Stream           bool              `json:"stream"`
	StatusCode       int               `json:"status_code"`
	ErrorCode        string            `json:"error_code,omitempty"`
	PromptTokens     int64             `json:"prompt_tokens"`
	CompletionTokens int64             `json:"completion_tokens"`
	TotalTokens      int64             `json:"total_tokens"`
	ReasoningTokens  int64             `json:"reasoning_tokens"`
	LatencyMS        int64             `json:"latency_ms"`
	TTFTMS           int64             `json:"ttft_ms,omitempty"`
	RawRequest       string            `json:"raw_request,omitempty"`
	RawResponse      string            `json:"raw_response,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`

	// Input-token compression metrics (see internal/compress). When
	// CompressionApplied is false the token fields are zero and the dashboard
	// shows "-". CompressionRatio is CompressedTokens/OriginalTokens.
	CompressionApplied bool    `json:"compression_applied"`
	OriginalTokens     int64   `json:"original_tokens,omitempty"`
	CompressedTokens   int64   `json:"compressed_tokens,omitempty"`
	CompressionRatio   float64 `json:"compression_ratio,omitempty"`
}

// AuditEvent is a record of an admin action.
type AuditEvent struct {
	ID         string         `json:"id"`
	AdminUser  string         `json:"admin_user"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id,omitempty"`
	OldValue   map[string]any `json:"old_value,omitempty"`
	NewValue   map[string]any `json:"new_value,omitempty"`
	IP         string         `json:"ip,omitempty"`
	UserAgent  string         `json:"user_agent,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// LogQuery filters request_logs.
type LogQuery struct {
	RequestID  string
	APIKeyID   string
	ClientIP   string
	Model      string
	BackendID  string
	Endpoint   string
	StatusCode int
	ErrorCode  string
	Stream     *bool
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Offset     int
}

// AuditQuery filters audit_logs.
type AuditQuery struct {
	AdminUser  string
	Action     string
	TargetType string
	TargetID   string
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Offset     int
}

// Stats aggregates totals over a window.
type Stats struct {
	TotalRequests    int64                   `json:"total_requests"`
	SuccessTotal     int64                   `json:"success_total"`
	ErrorTotal       int64                   `json:"error_total"`
	PromptTokens     int64                   `json:"prompt_tokens"`
	CompletionTokens int64                   `json:"completion_tokens"`
	TotalTokens      int64                   `json:"total_tokens"`
	ByModel          map[string]ModelStat    `json:"by_model"`
	ByBackend        map[string]BackendStat  `json:"by_backend"`
	ByAPIKey         map[string]KeyStat      `json:"by_api_key"`
	ByClientIP       map[string]ClientIPStat `json:"by_client_ip"`

	// Compression aggregates over the window. AvgCompressionRatio is computed
	// from summed token counts (CompressedTokens/OriginalTokens), not by
	// averaging per-row ratios, so large payloads are weighted correctly.
	CompressedRequests  int64   `json:"compressed_requests"`
	OriginalTokens      int64   `json:"original_tokens"`
	CompressedTokens    int64   `json:"compressed_tokens"`
	TokensSaved         int64   `json:"tokens_saved"`
	AvgCompressionRatio float64 `json:"avg_compression_ratio"`
}

type ModelStat struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	Tokens   int64 `json:"tokens"`
}

type BackendStat struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	Tokens   int64 `json:"tokens"`
}

type KeyStat struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	Tokens   int64 `json:"tokens"`
}

type ClientIPStat struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	Tokens   int64 `json:"tokens"`
}

// Store persists request and audit logs.
type Store interface {
	AppendRequest(ctx context.Context, rec *RequestLog) error
	QueryRequests(ctx context.Context, q LogQuery) ([]*RequestLog, error)
	StatsSince(ctx context.Context, since time.Time) (*Stats, error)

	AppendAudit(ctx context.Context, evt *AuditEvent) error
	QueryAudit(ctx context.Context, q AuditQuery) ([]*AuditEvent, error)

	Close() error
}

// Memory is a bounded in-memory log store (oldest-first eviction).
type Memory struct {
	mu       sync.RWMutex
	requests []*RequestLog
	audit    []*AuditEvent
	maxRecs  int
}

func NewMemory(maxRecords int) *Memory {
	if maxRecords <= 0 {
		maxRecords = 50000
	}
	return &Memory{maxRecs: maxRecords}
}

func (m *Memory) AppendRequest(_ context.Context, rec *RequestLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, rec)
	if len(m.requests) > m.maxRecs {
		evict := m.maxRecs / 10
		if evict < 1 {
			evict = 1
		}
		// Copy into a fresh slice so the old backing array (and the evicted
		// records) become garbage-collectable. Reslicing alone retains them.
		retained := make([]*RequestLog, len(m.requests)-evict)
		copy(retained, m.requests[evict:])
		m.requests = retained
	}
	return nil
}

func (m *Memory) QueryRequests(_ context.Context, q LogQuery) ([]*RequestLog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	out := make([]*RequestLog, 0, limit)
	// iterate newest-first
	for i := len(m.requests) - 1; i >= 0 && len(out) < limit+offset; i-- {
		r := m.requests[i]
		if !matchLog(r, q) {
			continue
		}
		out = append(out, r)
	}
	if offset >= len(out) {
		return nil, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) StatsSince(_ context.Context, since time.Time) (*Stats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := &Stats{
		ByModel:    make(map[string]ModelStat),
		ByBackend:  make(map[string]BackendStat),
		ByAPIKey:   make(map[string]KeyStat),
		ByClientIP: make(map[string]ClientIPStat),
	}
	for _, r := range m.requests {
		if r.CreatedAt.Before(since) {
			continue
		}
		s.TotalRequests++
		if r.StatusCode >= 200 && r.StatusCode < 400 {
			s.SuccessTotal++
		} else {
			s.ErrorTotal++
		}
		s.PromptTokens += r.PromptTokens
		s.CompletionTokens += r.CompletionTokens
		s.TotalTokens += r.TotalTokens

		if r.CompressionApplied {
			s.CompressedRequests++
			s.OriginalTokens += r.OriginalTokens
			s.CompressedTokens += r.CompressedTokens
		}

		ms := s.ByModel[r.Model]
		ms.Requests++
		ms.Tokens += r.TotalTokens
		if r.StatusCode >= 400 {
			ms.Errors++
		}
		s.ByModel[r.Model] = ms

		bs := s.ByBackend[r.BackendID]
		bs.Requests++
		bs.Tokens += r.TotalTokens
		if r.StatusCode >= 400 {
			bs.Errors++
		}
		s.ByBackend[r.BackendID] = bs

		ks := s.ByAPIKey[r.APIKeyID]
		ks.Requests++
		ks.Tokens += r.TotalTokens
		if r.StatusCode >= 400 {
			ks.Errors++
		}
		s.ByAPIKey[r.APIKeyID] = ks

		if r.ClientIP != "" {
			cs := s.ByClientIP[r.ClientIP]
			cs.Requests++
			cs.Tokens += r.TotalTokens
			if r.StatusCode >= 400 {
				cs.Errors++
			}
			s.ByClientIP[r.ClientIP] = cs
		}
	}
	finalizeCompressionStats(s)
	return s, nil
}

// finalizeCompressionStats derives TokensSaved and AvgCompressionRatio from the
// summed token counts. Shared by all store implementations.
func finalizeCompressionStats(s *Stats) {
	s.TokensSaved = s.OriginalTokens - s.CompressedTokens
	if s.OriginalTokens > 0 {
		s.AvgCompressionRatio = float64(s.CompressedTokens) / float64(s.OriginalTokens)
	}
}

func (m *Memory) AppendAudit(_ context.Context, evt *AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, evt)
	if len(m.audit) > m.maxRecs {
		evict := m.maxRecs / 10
		if evict < 1 {
			evict = 1
		}
		retained := make([]*AuditEvent, len(m.audit)-evict)
		copy(retained, m.audit[evict:])
		m.audit = retained
	}
	return nil
}

func (m *Memory) QueryAudit(_ context.Context, q AuditQuery) ([]*AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := make([]*AuditEvent, 0, limit)
	for i := len(m.audit) - 1; i >= 0 && len(out) < limit+q.Offset; i-- {
		a := m.audit[i]
		if q.AdminUser != "" && a.AdminUser != q.AdminUser {
			continue
		}
		if q.Action != "" && a.Action != q.Action {
			continue
		}
		if q.TargetType != "" && a.TargetType != q.TargetType {
			continue
		}
		if q.TargetID != "" && a.TargetID != q.TargetID {
			continue
		}
		if q.Since != nil && a.CreatedAt.Before(*q.Since) {
			continue
		}
		if q.Until != nil && a.CreatedAt.After(*q.Until) {
			continue
		}
		out = append(out, a)
	}
	if q.Offset >= len(out) {
		return nil, nil
	}
	out = out[q.Offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Close() error { return nil }

func matchLog(r *RequestLog, q LogQuery) bool {
	if q.RequestID != "" && r.RequestID != q.RequestID {
		return false
	}
	if q.APIKeyID != "" && r.APIKeyID != q.APIKeyID {
		return false
	}
	if q.ClientIP != "" && r.ClientIP != q.ClientIP {
		return false
	}
	if q.Model != "" && r.Model != q.Model {
		return false
	}
	if q.BackendID != "" && r.BackendID != q.BackendID {
		return false
	}
	if q.Endpoint != "" && r.Endpoint != q.Endpoint {
		return false
	}
	if q.StatusCode != 0 && r.StatusCode != q.StatusCode {
		return false
	}
	if q.ErrorCode != "" && r.ErrorCode != q.ErrorCode {
		return false
	}
	if q.Stream != nil && r.Stream != *q.Stream {
		return false
	}
	if q.Since != nil && r.CreatedAt.Before(*q.Since) {
		return false
	}
	if q.Until != nil && r.CreatedAt.After(*q.Until) {
		return false
	}
	return true
}
