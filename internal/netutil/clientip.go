// Package netutil provides shared network helpers, in particular a single
// trusted client-IP extractor used by auth, request logs and stats.
package netutil

import (
	"net"
	"net/http"
	"strings"
)

// Extractor resolves the originating client IP for a request.
//
// XFF / X-Real-IP headers are only honored when the immediate peer
// (r.RemoteAddr) is itself contained in TrustedProxies (a list of CIDRs
// or exact IPs). Otherwise the headers are ignored so a malicious client
// can't forge their own IP.
type Extractor struct {
	trusted []*net.IPNet
}

// NewExtractor builds an Extractor from a list of CIDR or single-IP strings.
// Invalid entries are silently dropped. An empty list means: never trust
// forwarded headers (only RemoteAddr is used).
func NewExtractor(trusted []string) *Extractor {
	nets := make([]*net.IPNet, 0, len(trusted))
	for _, t := range trusted {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !strings.Contains(t, "/") {
			ip := net.ParseIP(t)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				t = t + "/32"
			} else {
				t = t + "/128"
			}
		}
		_, ipNet, err := net.ParseCIDR(t)
		if err != nil {
			continue
		}
		nets = append(nets, ipNet)
	}
	return &Extractor{trusted: nets}
}

// ClientIP returns the originating client IP as a string. Returns the
// canonical textual form (no port). If the request has no usable address
// it returns the empty string.
//
// XFF is parsed using the rightmost-untrusted algorithm: starting from
// the last entry, skip every IP that is itself a trusted proxy; the
// first non-trusted IP is the real client. This prevents a malicious
// client from forging the leftmost XFF entry — even when a trusted proxy
// is configured to append (rather than overwrite) the header.
func (e *Extractor) ClientIP(r *http.Request) string {
	peer := remoteIP(r.RemoteAddr)
	if e == nil || !e.isTrustedIP(peer) {
		// Immediate peer is not trusted: ignore all forwarded headers.
		return peer
	}
	// Build the candidate chain from XFF (left-to-right reflects the
	// forwarding order: client, proxy1, proxy2, ...). The current peer
	// is implicit at the right end.
	xff := splitForwardedFor(r.Header.Get("X-Forwarded-For"))
	// Walk right-to-left, skipping trusted IPs. The first untrusted IP
	// is the real client.
	for i := len(xff) - 1; i >= 0; i-- {
		ip := xff[i]
		if ip == "" {
			continue
		}
		if e.isTrustedIP(ip) {
			continue
		}
		return ip
	}
	// All XFF entries were trusted (or XFF was empty). Honor X-Real-IP
	// only when set by the trusted hop, and only when XFF didn't supply
	// a usable answer.
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		if parsed := net.ParseIP(real); parsed != nil {
			if !e.isTrustedIP(parsed.String()) {
				return parsed.String()
			}
		}
	}
	// Last resort: the immediate peer (trusted) itself.
	return peer
}

func (e *Extractor) isTrustedIP(ipStr string) bool {
	if e == nil || len(e.trusted) == 0 || ipStr == "" {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range e.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// MatchAny reports whether ipStr falls inside any of the supplied
// CIDRs / exact IPs. Used to evaluate API-key allowed/denied IP lists.
func MatchAny(ipStr string, list []string) bool {
	if ipStr == "" || len(list) == 0 {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "/") {
			other := net.ParseIP(raw)
			if other == nil {
				continue
			}
			if other.Equal(ip) {
				return true
			}
			continue
		}
		_, n, err := net.ParseCIDR(raw)
		if err != nil {
			continue
		}
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
		return host
	}
	if ip := net.ParseIP(remoteAddr); ip != nil {
		return ip.String()
	}
	return remoteAddr
}

// splitForwardedFor returns a list of canonical IP strings from the
// X-Forwarded-For header in original order (leftmost = client per
// convention). Entries that don't parse as an IP are dropped so the
// rightmost-untrusted scan doesn't pick up garbage.
func splitForwardedFor(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		// XFF can carry "ip:port" (rare) — strip the port.
		if host, _, err := net.SplitHostPort(s); err == nil {
			s = host
		}
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip.String())
		}
	}
	return out
}
