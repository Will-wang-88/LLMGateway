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
