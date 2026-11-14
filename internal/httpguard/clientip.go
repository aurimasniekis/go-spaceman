package httpguard

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP is the request's client address: the TCP peer, or, when the peer is in trusted (a reverse proxy), the
// right-most X-Forwarded-For entry that isn't. Every X-Forwarded-For line counts, joined in order (RFC 9110 §5.3), so
// a proxy that appends its own line instead of extending the first can't be skipped over. The walk stops at the first
// unparsable entry and falls back to the peer. The result is unmapped; it is invalid when the peer address itself is
// unparsable.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !inPrefixes(peer, trusted) {
		return peer
	}
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		if a = a.Unmap(); !inPrefixes(a, trusted) {
			return a
		}
	}
	return peer
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
