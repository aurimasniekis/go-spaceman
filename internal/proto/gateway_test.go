package proto

import (
	"strings"
	"testing"
)

// SM-GW-003 (ADR-0060 decision 3): the node's own check of the address it sends a run's token to.
func TestGatewayOrigin(t *testing.T) {
	t.Parallel()
	ok := map[GatewayLaunch]string{
		{Kind: GatewayHarborPilot, BaseURL: "https://hp.example.com"}:                            "https://hp.example.com",
		{Kind: GatewayHarborPilot, BaseURL: "https://hp.example.com/gw/"}:                        "https://hp.example.com/gw",
		{Kind: GatewayHarborPilot, BaseURL: " https://hp.example.com:8443 "}:                     "https://hp.example.com:8443",
		{Kind: GatewayHarborPilot, BaseURL: "http://127.0.0.1:4000", AllowInsecureHTTP: true}:    "http://127.0.0.1:4000",
		{Kind: GatewayHarborPilot, BaseURL: "http://hp.internal:4000/", AllowInsecureHTTP: true}: "http://hp.internal:4000",
	}
	for g, want := range ok {
		if got, err := g.Origin(); err != nil || got != want {
			t.Errorf("%+v: %q %v, want %q", g, got, err, want)
		}
	}
	bad := map[GatewayLaunch]string{
		{Kind: GatewayHarborPilot, BaseURL: "http://127.0.0.1:4000"}:        "plain http",
		{Kind: GatewayHarborPilot, BaseURL: "http://localhost:4000"}:        "plain http",
		{Kind: GatewayHarborPilot, BaseURL: "hp.example.com"}:               "absolute",
		{Kind: GatewayHarborPilot, BaseURL: "ftp://hp.example.com"}:         "absolute",
		{Kind: GatewayHarborPilot, BaseURL: "https://u:p@hp.example.com"}:   "credentials",
		{Kind: GatewayHarborPilot, BaseURL: "https://hp.example.com/?a"}:    "query",
		{Kind: GatewayHarborPilot, BaseURL: "https://hp.example.com/#x"}:    "fragment",
		{Kind: GatewayHarborPilot, BaseURL: ""}:                             "absolute",
		{Kind: GatewayHarborPilot, BaseURL: "https://:443"}:                 "name a host",
		{Kind: GatewayHarborPilot, BaseURL: "https://hp.example.com:0"}:     "port",
		{Kind: GatewayHarborPilot, BaseURL: "https://hp.example.com:99999"}: "port",
		{Kind: "other", BaseURL: "https://hp.example.com"}:                  "unknown kind",
	}
	for g, want := range bad {
		if _, err := g.Origin(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", g, err, want)
		}
	}
}
