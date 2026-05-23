package ratelimit

import "testing"

func TestCheckAndReserveAtomic(t *testing.T) {
	l := New()
	// 3 requests/min, no token limit, no day limits.
	for i := 0; i < 3; i++ {
		if c := l.CheckAndReserve("k1", 3, 0, 0, 0); c != "" {
			t.Fatalf("call %d should pass, got %q", i, c)
		}
	}
	if c := l.CheckAndReserve("k1", 3, 0, 0, 0); c != "rate_limit_exceeded" {
		t.Errorf("4th call should hit rate_limit_exceeded, got %q", c)
	}
}

func TestCheckAndReserveTokenLimitDoesNotCountRequest(t *testing.T) {
	// When the token check fails, the request bucket must NOT be incremented
	// (this was the H4 atomicity bug from review).
	l := New()
	// First, accumulate tokens to exceed the limit.
	l.AddTokens("k", 100)
	// Token limit is now reached. Subsequent CheckAndReserve must fail on
	// tokens, and the request count must remain 0.
	if c := l.CheckAndReserve("k", 1000, 50, 0, 0); c != "token_rate_limit_exceeded" {
		t.Fatalf("expected token_rate_limit_exceeded, got %q", c)
	}
	// Now a fresh check WITHOUT the token limit must still pass - i.e. the
	// previous failed call did not eat into the request budget.
	if c := l.CheckAndReserve("k", 1, 0, 0, 0); c != "" {
		t.Errorf("rate budget should not have been consumed by failed token check, got %q", c)
	}
	// Second call exhausts the rpm=1 budget.
	if c := l.CheckAndReserve("k", 1, 0, 0, 0); c != "rate_limit_exceeded" {
		t.Errorf("expected rate_limit_exceeded, got %q", c)
	}
}

func TestCheckAndReserveDailyLimit(t *testing.T) {
	l := New()
	for i := 0; i < 5; i++ {
		if c := l.CheckAndReserve("k", 0, 0, 5, 0); c != "" {
			t.Fatalf("call %d should pass, got %q", i, c)
		}
	}
	if c := l.CheckAndReserve("k", 0, 0, 5, 0); c != "daily_request_limit_exceeded" {
		t.Errorf("expected daily_request_limit_exceeded, got %q", c)
	}
}

func TestConcurrencyAcquireRelease(t *testing.T) {
	c := NewConcurrency()
	if !c.Acquire("k", 2) || !c.Acquire("k", 2) {
		t.Fatal("first two should succeed")
	}
	if c.Acquire("k", 2) {
		t.Fatal("third should be rejected")
	}
	c.Release("k", 2)
	if !c.Acquire("k", 2) {
		t.Fatal("after release, should succeed again")
	}
}

func TestConcurrencyUnlimitedDoesNotTrack(t *testing.T) {
	c := NewConcurrency()
	for i := 0; i < 100; i++ {
		if !c.Acquire("k", 0) {
			t.Fatalf("unlimited acquire %d failed", i)
		}
	}
	// Get should still return 0 because limit<=0 isn't recorded
	// (prevents unbounded map growth).
	if g := c.Get("k"); g != 0 {
		t.Errorf("unlimited keys should not be tracked, got %d", g)
	}
	// Release should be a no-op.
	c.Release("k", 0)
}
