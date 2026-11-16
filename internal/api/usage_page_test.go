package api

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

func TestPageSeriesCursor(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s := &usage.Series{Bucket: "hour", GroupBy: "role"}
	for h := 0; h < 3; h++ {
		for _, g := range []string{"", "a", "b"} {
			ts := base.Add(time.Duration(h) * time.Hour)
			s.Rows = append(s.Rows, usage.Row{BucketStart: &ts, Group: g, TotalTokens: 1})
		}
	}
	var seen []usage.Row
	cur := ""
	for i := 0; i < 20; i++ {
		pg, err := pageSeries(s, Page{Cursor: cur, Limit: 4})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, pg.Items...)
		if pg.NextCursor == "" {
			break
		}
		cur = pg.NextCursor
	}
	if len(seen) != 9 {
		t.Fatalf("pages returned %d rows, want 9", len(seen))
	}
	for i := range seen {
		if seen[i].BucketStart != s.Rows[i].BucketStart || seen[i].Group != s.Rows[i].Group {
			t.Fatalf("row %d out of order", i)
		}
	}
	if _, err := pageSeries(s, Page{Cursor: "garbage", Limit: 4}); err == nil {
		t.Fatal("bad cursor accepted")
	}
}
