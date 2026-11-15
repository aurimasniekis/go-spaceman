// Package tghost holds the Mini App host rules shared by central's configuration (telegram.miniapp_host), the bot's
// miniapp_host and central's host routing (ADR-0083 §3.4.1): what a valid Mini App host is, and how a request's Host
// header is compared with it.
package tghost

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// Of is a Host header's host name: lower case, without the port and one trailing dot. "tg.example.com." is the same
// host to DNS, so it must get the Mini App host's allowlist, never the main host's routes under the Mini App name.
func Of(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		hostport = h
	}
	return strings.ToLower(strings.TrimSuffix(hostport, "."))
}

// reNumericLabel is a last label a browser's URL parser takes for a number (decimal, octal or 0x hex), which makes
// the whole host an IPv4 address or invalid.
var reNumericLabel = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]*)$`)

// reHostLabel is one DNS label in lower case.
var reHostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Check validates a Mini App host (plan §3.4.1): a bare lower-case DNS name with at least two labels, no scheme, port,
// path or trailing dot, not an IP address, and not the host of central's public URL (the Mini App needs its own
// origin, so no cookie of the main host reaches it). An empty host is valid (no Mini App). The error is a message
// for the field, without the field's name.
func Check(host, publicURL string) error {
	if host == "" {
		return nil
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return errors.New("must be a DNS name, not an IP address")
	}
	if strings.ContainsAny(host, ":/?#@ ") {
		return errors.New("must be a bare host name, without scheme, port or path (e.g. tg.central.example.com)")
	}
	if host != strings.ToLower(host) {
		return errors.New("must be lower case")
	}
	labels := strings.Split(host, ".")
	if len(host) > 253 || len(labels) < 2 {
		return errors.New("must be a fully qualified DNS name of at most 253 characters (e.g. tg.central.example.com)")
	}
	for _, l := range labels {
		if !reHostLabel.MatchString(l) {
			return errors.New("is not a valid DNS name: each label is 1-63 of a-z, 0-9 and '-', not starting or ending with '-'")
		}
	}
	if reNumericLabel.MatchString(labels[len(labels)-1]) {
		return errors.New("must not end in a number: browsers read 127.1, 0x7f.1 or 0177.0.0.1 as an IPv4 address and refuse tg.123")
	}
	if u, err := url.Parse(publicURL); err == nil && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), host) {
		return errors.New("must differ from the host of central's public URL: the Mini App needs its own origin")
	}
	return nil
}
