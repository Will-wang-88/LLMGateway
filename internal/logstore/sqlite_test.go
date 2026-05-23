package logstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	rec := &RequestLog{
		ID: "1", RequestID: "req-1", APIKeyID: "k1", APIKeyName: "team-a",
		Model: "llama-3.1-70b", InternalModel: "llama-3.1-70b", BackendID: "be-1",
		Endpoint: "/chat/completions", Stream: true, StatusCode: 200,
		PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, ReasoningTokens: 5,
		LatencyMS: 250, TTFTMS: 120, CreatedAt: time.Now().UTC(),
	}
	if err := db.AppendRequest(ctx, rec); err != nil {
		t.Fatal(err)
	}

	logs, err := db.QueryRequests(ctx, LogQuery{Model: "llama-3.1-70b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}
	if logs[0].PromptTokens != 10 || logs[0].TotalTokens != 30 {
		t.Errorf("token roundtrip wrong: %+v", logs[0])
	}
	if !logs[0].Stream {
		t.Error("expected stream true")
	}

	stats, err := db.StatsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 1 || stats.SuccessTotal != 1 {
		t.Errorf("unexpected stats: %+v", stats)
	}
	if stats.ByModel["llama-3.1-70b"].Tokens != 30 {
		t.Errorf("by-model tokens wrong: %+v", stats.ByModel)
	}

	if err := db.AppendAudit(ctx, &AuditEvent{
		ID: "a1", AdminUser: "alice", Action: "backend.create",
		TargetType: "backend", TargetID: "be-1",
		NewValue:  map[string]any{"weight": 2.0},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	events, err := db.QueryAudit(ctx, AuditQuery{TargetType: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(events))
	}
	if events[0].AdminUser != "alice" || events[0].NewValue["weight"] != 2.0 {
		t.Errorf("audit roundtrip wrong: %+v", events[0])
	}
}

func TestSQLitePurge(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	// old record
	_ = db.AppendRequest(ctx, &RequestLog{ID: "old", Model: "m", Endpoint: "/e", CreatedAt: now.Add(-72 * time.Hour)})
	// fresh record
	_ = db.AppendRequest(ctx, &RequestLog{ID: "new", Model: "m", Endpoint: "/e", CreatedAt: now})
	if err := db.Purge(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	logs, _ := db.QueryRequests(ctx, LogQuery{})
	if len(logs) != 1 || logs[0].ID != "new" {
		t.Errorf("expected only 'new' record after purge, got %+v", logs)
	}
}
