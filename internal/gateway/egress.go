package gateway

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// Egress policy (ADR-0023). The check runs in the dialer's Control hook, on the address actually being connected to
// after DNS resolution, so a name that resolves (or re-resolves) to a forbidden address is refused too. Environment
// proxies are ignored (a proxy would hide the destination) and redirects are never followed.

var (
	// Always refused, whatever the server's settings: link-local (cloud metadata lives at 169.254.169.254), multicast,
	// unspecified, "this network" and broadcast.
	alwaysDenied = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("255.255.255.255/32"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("ff00::/8"),
		netip.MustParsePrefix("::/128"),
	}
	// Refused unless the server allows private networks: loopback, RFC 1918, carrier-grade NAT, benchmarking, IETF and
	// reserved ranges, unique-local IPv6, and the IPv6 translation prefixes that can reach IPv4 private space.
	privateRanges = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("2002::/16"),
	}
)

// ErrEgressDenied is returned when the egress policy refuses a destination.
var ErrEgressDenied = errors.New("the destination address is not allowed by the egress policy")

// allowedAddr reports whether the gateway may connect to ip.
func allowedAddr(ip netip.Addr, allowPrivate bool) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, p := range alwaysDenied {
		if p.Contains(ip) {
			return false
		}
	}
	if allowPrivate {
		return true
	}
	for _, p := range privateRanges {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// control is the dialer's Control hook: address is "ip:port" after resolution.
func control(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		if network != "tcp4" && network != "tcp6" {
			return fmt.Errorf("%w: network %s", ErrEgressDenied, network)
		}
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !allowedAddr(ap.Addr(), allowPrivate) {
			return ErrEgressDenied
		}
		return nil
	}
}

// httpClient returns the client for one server: policy-checked dialing, no proxy, no redirects, the server's headers
// added to every request, and response bodies bounded.
func httpClient(allowPrivate bool, headers map[string]string, maxBody int64) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: control(allowPrivate)}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            d.DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        90 * time.Second,
	}
	return &http.Client{
		Transport:     &headerTransport{base: tr, headers: headers, maxBody: maxBody},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("upstream redirects are not followed") },
	}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	maxBody int64
}

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	res, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	res.Body = &limitedBody{rc: res.Body, left: t.maxBody}
	return res, nil
}

// ErrTooLarge is returned when an upstream response exceeds the server's limit.
var ErrTooLarge = errors.New("the upstream response is larger than this server's limit")

type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
