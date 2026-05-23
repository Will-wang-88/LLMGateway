package quota

import (
	"testing"
)

func TestQuotaDailyLimits(t *testing.T) {
	m := New()
	for i := 0; i < 5; i++ {
		if c := m.Check("k1", 5, 0, 0, 0); c != "" {
			t.Fatalf("expected allowed, got %s", c)
		}
		m.AddRequest("k1")
	}
	if c := m.Check("k1", 5, 0, 0, 0); c == "" {
		t.Errorf("expected daily_request_quota_exceeded after 5 requests")
	}
}

func TestQuotaTokensIncrement(t *testing.T) {
	m := New()
	m.AddTokens("k1", 100)
	m.AddTokens("k1", 50)
	dayR, dayT, monR, monT := m.Usage("k1")
	if dayT != 150 || monT != 150 {
		t.Errorf("expected 150 tokens day/month, got day=%d month=%d", dayT, monT)
	}
	if dayR != 0 || monR != 0 {
		t.Errorf("unexpected request counters: day=%d month=%d", dayR, monR)
	}
}

func TestQuotaUnlimited(t *testing.T) {
	m := New()
	for i := 0; i < 100; i++ {
		if c := m.Check("k", 0, 0, 0, 0); c != "" {
			t.Fatal("expected unlimited")
		}
		m.AddRequest("k")
		m.AddTokens("k", 1000)
	}
}
