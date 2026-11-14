package link

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// SM-TG-001 (TG-4b, review PR #86): a reload that cannot read the store empties the cache (fail closed), so a link
// revoked while the database is unreachable never keeps acting, and the cache tells the limiter the link left.
func TestCacheFailsClosedWhenTheStoreFails(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t)
	c := NewCache(st, eventbus.New(), nil, 0)
	var removed []string
	c.OnRemove = func(id string) { removed = append(removed, id) }
	c.byTG[7] = updates.Link{ID: "tgl_1", UserID: "usr_1", ChatID: 7}
	c.byID["tgl_1"] = 7
	if _, ok := c.Lookup(7); !ok {
		t.Fatal("the seeded link is not cached")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.reloadAll(context.Background()); err == nil {
		t.Fatal("reloading from a closed store reported no error")
	}
	if l, ok := c.Lookup(7); ok {
		t.Fatalf("the cache kept %+v after a failed reload (fail open)", l)
	}
	if len(removed) != 1 || removed[0] != "tgl_1" {
		t.Fatalf("removal notices: %v", removed)
	}
}
