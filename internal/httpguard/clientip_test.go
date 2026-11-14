package httpguard

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// ClientIP: the peer unless it is a trusted proxy; then the right-most untrusted X-Forwarded-For entry over every
// line of the header, stopping at an unparsable entry.
func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		remote string
		xff    []string
		want   string
	}{
		{"203.0.113.9:443", nil, "203.0.113.9"},
		{"203.0.113.9:443", []string{"198.51.100.1"}, "203.0.113.9"}, // an untrusted peer can't claim an address
		{"[::ffff:203.0.113.9]:443", nil, "203.0.113.9"},
		{"10.1.1.1:443", nil, "10.1.1.1"},
		{"10.1.1.1:443", []string{"198.51.100.1, 10.2.2.2"}, "198.51.100.1"},
		{"10.1.1.1:443", []string{"198.51.100.1", "203.0.113.5"}, "203.0.113.5"}, // the last line counts
		{"10.1.1.1:443", []string{"203.0.113.5", "198.51.100.1, 10.2.2.2"}, "198.51.100.1"},
		{"10.1.1.1:443", []string{"198.51.100.1, junk, 10.2.2.2"}, "10.1.1.1"},
		{"10.1.1.1:443", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"not-an-address", nil, "invalid IP"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := ClientIP(r, trusted).String(); got != c.want {
			t.Errorf("remote %s xff %q: %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}
