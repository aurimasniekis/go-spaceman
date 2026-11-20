package cli

import (
	"bytes"
	"strings"
	"testing"
)

// A switch-back shows its cause in the CAUSE column, apart from a usage-limit move.
func TestSwitchesCauseColumn(t *testing.T) {
	t.Parallel()
	body := []byte(`{"items":[{"id":"asw_back","role":"implementor","cause":"switch_back","from_account_id":"acc_b","to_account_id":"acc_a","state":"succeeded"},{"id":"asw_limit","role":"implementor","cause":"usage_limit","from_account_id":"acc_a","to_account_id":"acc_b","state":"succeeded"}],"next_cursor":""}`)
	var b bytes.Buffer
	if err := render(&b, FormatTable, body, view{items: "items", cols: switchCols}); err != nil {
		t.Fatal(err)
	}
	var head string
	rows := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		switch {
		case strings.HasPrefix(line, "ID"):
			head = line
		case strings.Contains(line, "asw_back"):
			rows["back"] = line
		case strings.Contains(line, "asw_limit"):
			rows["limit"] = line
		}
	}
	if !strings.Contains(head, "CAUSE") {
		t.Fatalf("no CAUSE column:\n%s", b.String())
	}
	if !strings.Contains(rows["back"], "switch_back") || !strings.Contains(rows["limit"], "usage_limit") {
		t.Fatalf("causes not shown:\n%s", b.String())
	}
}
