package proto

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// Origin checks the gateway address as a node does before it hands a run its token (ADR-0060 decision 3), and returns
// it without a trailing slash. It must be an absolute https URL without credentials, a query or a fragment; plain http
// only with AllowInsecureHTTP. There is no loopback exemption: central's checks may have run against another host's
// loopback, and a central that skipped them must not make a node send a token in clear text.
func (g GatewayLaunch) Origin() (string, error) {
	if g.Kind != GatewayHarborPilot {
		return "", errors.New("gateway: unknown kind")
	}
	u, err := url.Parse(strings.TrimSpace(g.BaseURL))
	switch {
	case err != nil || u.Host == "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http"):
		return "", errors.New("gateway: base_url must be an absolute http(s) URL")
	case u.Hostname() == "" || !validPort(u.Port()):
		return "", errors.New("gateway: base_url must name a host and, if it has one, a port from 1 to 65535")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("gateway: base_url must not contain credentials, a query or a fragment")
	case u.Scheme == "http" && !g.AllowInsecureHTTP:
		return "", errors.New("gateway: base_url uses plain http, and insecure HTTP is not allowed: the run's token would cross the network in clear text")
	case strings.ContainsAny(u.String(), "\"\\\n\r\t "):
		return "", errors.New("gateway: base_url contains characters that are not allowed")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// validPort reports whether a URL's port is absent or a number from 1 to 65535.
func validPort(p string) bool {
	if p == "" {
		return true
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535 && p[0] != '+'
}
