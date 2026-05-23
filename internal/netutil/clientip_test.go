package netutil

import (
	"net/http/httptest"
	"testing"
)

func TestExtractorRejectsSpoofedXFFFromUntrustedPeer(t *testing.T) {
	e := NewExtractor(nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:12345"
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Real-IP", "10.0.0.2")
	if got := e.ClientIP(r); got != "203.0.113.5" {
		t.Errorf("expected RemoteAddr %q, got %q", "203.0.113.5", got)
	}
}

func TestExtractorHonorsXFFFromTrustedProxy(t *testing.T) {
	e := NewExtractor([]string{"10.0.0.0/8"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.5.6.7:443"
	r.Header.Set("X-Forwarded-For", "198.51.100.10, 10.5.6.7")
	if got := e.ClientIP(r); got != "198.51.100.10" {
		t.Errorf("expected first XFF entry, got %q", got)
	}
}

func TestExtractorFallsBackToXRealIP(t *testing.T) {
	e := NewExtractor([]string{"10.0.0.0/8"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.5.6.7:443"
	r.Header.Set("X-Real-IP", "198.51.100.50")
	if got := e.ClientIP(r); got != "198.51.100.50" {
		t.Errorf("expected XRealIP value, got %q", got)
	}
}

// P0-3 (review): rightmost-untrusted XFF is the only safe algorithm
// when a trusted proxy appends (rather than overwrites) the header.
func TestClientIPExtractorUsesRightmostUntrustedXFF(t *testing.T) {
	e := NewExtractor([]string{"10.0.0.0/8", "192.168.0.0/16"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.5.6.7:443"
	// Chain: real-client -> CDN-edge -> internal-LB -> gateway.
	// Internal hops are trusted; the real client is the only untrusted IP.
	r.Header.Set("X-Forwarded-For", "203.0.113.42, 192.168.1.5, 10.5.6.7")
	if got := e.ClientIP(r); got != "203.0.113.42" {
		t.Errorf("expected rightmost-untrusted 203.0.113.42, got %q", got)
	}
}

// Malicious client prepends its own value; trusted proxy appends the
// actual hop. The leftmost value MUST be ignored.
func TestClientIPExtractorIgnoresSpoofedLeftmostXFFBehindTrustedProxy(t *testing.T) {
	e := NewExtractor([]string{"10.0.0.0/8"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.5.6.7:443"
	// Attacker sent: XFF "10.0.0.5" (a "trusted internal IP" they hope
	// the gateway will mistake for the client). Trusted proxy appended
	// the real attacker IP (203.0.113.9).
	r.Header.Set("X-Forwarded-For", "10.0.0.5, 203.0.113.9")
	if got := e.ClientIP(r); got != "203.0.113.9" {
		t.Errorf("expected real attacker IP 203.0.113.9, got %q", got)
	}
}

// All XFF entries are trusted proxies (e.g. multi-hop internal mesh
// before reaching anything untrusted) — the extractor must fall back to
// the trusted peer rather than blindly returning a trusted hop.
func TestClientIPExtractorAllTrustedFallsBack(t *testing.T) {
	e := NewExtractor([]string{"10.0.0.0/8"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.5.6.7:443"
	r.Header.Set("X-Forwarded-For", "10.1.1.1, 10.2.2.2")
	if got := e.ClientIP(r); got != "10.5.6.7" {
		t.Errorf("expected fallback to peer when XFF is all-trusted, got %q", got)
	}
}

func TestMatchAnyCIDRAndExact(t *testing.T) {
	cases := []struct {
		ip   string
		list []string
		want bool
	}{
		{"127.0.0.1", []string{"127.0.0.1/32"}, true},
		{"127.0.0.2", []string{"127.0.0.1/32"}, false},
		{"127.0.0.5", []string{"127.0.0.0/24"}, true},
		{"10.5.6.7", []string{"10.0.0.0/8"}, true},
		{"2001:db8::1", []string{"2001:db8::/32"}, true},
		{"2001:db8::1", []string{}, false},
		{"10.1.2.3", []string{"10.1.2.3"}, true},
		{"", []string{"10.0.0.0/8"}, false},
		{"not-an-ip", []string{"10.0.0.0/8"}, false},
	}
	for _, c := range cases {
		if got := MatchAny(c.ip, c.list); got != c.want {
			t.Errorf("MatchAny(%q, %v) = %v, want %v", c.ip, c.list, got, c.want)
		}
	}
}
