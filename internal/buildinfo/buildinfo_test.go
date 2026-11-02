package buildinfo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrint(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(&buf, false); err != nil || !strings.HasPrefix(buf.String(), "spaceman ") {
		t.Fatalf("text: %q %v", buf.String(), err)
	}
	buf.Reset()
	var info Info
	if err := Print(&buf, true); err != nil || json.Unmarshal(buf.Bytes(), &info) != nil || info.GoVersion == "" {
		t.Fatalf("json: %q %v", buf.String(), err)
	}
}
