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
func (e *Extractor) ClientIP(r *http.Request) string {
	peer := remoteIP(r.RemoteAddr)
	if e != nil && e.isTrustedIP(peer) {
		if ip := parseFirstForwardedFor(r.Header.Get("X-Forwarded-For")); ip != "" {
			return ip
		}
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			if parsed := net.ParseIP(ip); parsed != nil {
				return parsed.String()
			}
		}
	}
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

func parseFirstForwardedFor(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.Split(header, ",")
	first := strings.TrimSpace(parts[0])
	if first == "" {
		return ""
	}
	if ip := net.ParseIP(first); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(first); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return first
}
