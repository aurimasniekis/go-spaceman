package tghost

import (
	"strings"
	"testing"
)

// A Host header names the Mini App host in any case, with any port and one trailing dot; nothing else does.
func TestOf(t *testing.T) {
	for in, want := range map[string]string{"tg.example.com": "tg.example.com", "TG.Example.COM": "tg.example.com", "tg.example.com:8443": "tg.example.com",
		"TG.example.com.:80": "tg.example.com", "tg.example.com.": "tg.example.com", "tg.example.com..": "tg.example.com.", "[::1]:80": "::1", "": ""} {
		if got := Of(in); got != want {
			t.Errorf("Of(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheck(t *testing.T) {
	const public = "https://central.example.com"
	for _, ok := range []string{"", "tg.example.com", "tg.localhost", "tg.central.example.com"} {
		if err := Check(ok, public); err != nil {
			t.Errorf("Check(%q) = %v", ok, err)
		}
	}
	for host, want := range map[string]string{"127.0.0.1": "IP address", "[::1]": "IP address", "tg.example.com:443": "bare host name",
		"https://tg.example.com": "bare host name", "TG.example.com": "lower case", "localhost": "fully qualified", "tg.example.com.": "valid DNS name",
		"-tg.example.com": "valid DNS name", "tg.123": "end in a number", "central.example.com": "public URL"} {
		if err := Check(host, public); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v, want %q", host, err, want)
		}
	}
}
