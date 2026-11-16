package app

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
)

func TestMiniAppActionLink(t *testing.T) {
	const origin = "https://mini.example"
	if got := miniAppActionLink("", "spc_a", "hum_b"); got != "" {
		t.Errorf("no origin: got %q, want no link", got)
	}
	if got, want := miniAppActionLink(origin, "spc_a", "hum_b"), origin+"/spaces/spc_a/human?action=hum_b"; got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
	// The IDs are escaped the way the approval button's link always was.
	if got, want := miniAppActionLink(origin, "spc a", "x&y"), origin+"/spaces/spc%20a/human?action=x%26y"; got != want {
		t.Errorf("escaped link = %q, want %q", got, want)
	}
}

// With a Mini App origin, a human action card links to its page and a task card gets no link, so both the type filter
// and the action ID (not the space ID) are pinned.
func TestCardAppLinkHumanActionOnly(t *testing.T) {
	const origin = "https://mini.example"
	human := notify.Resource{Key: notify.Key{Type: notify.TypeHumanAction, ID: "hum_b"}, SpaceID: "spc_a"}
	if got, want := cardAppLink(origin, human.SpaceID, human.Key), origin+"/spaces/spc_a/human?action=hum_b"; got != want {
		t.Errorf("human card link = %q, want %q", got, want)
	}
	task := notify.Resource{Key: notify.Key{Type: notify.TypeTask, ID: "tsk_b"}, SpaceID: "spc_a"}
	if got := cardAppLink(origin, task.SpaceID, task.Key); got != "" {
		t.Errorf("task card link = %q, want none (only human actions have a page)", got)
	}
}

func TestCardAppURLOnlyForHumanActionsWithAHost(t *testing.T) {
	// A Central without a bot has no Mini App host, so no card offers a button.
	c := &Central{}
	human := notify.Resource{Key: notify.Key{Type: notify.TypeHumanAction, ID: "hum_b"}, SpaceID: "spc_a"}
	if got := c.cardAppURL(human.SpaceID, human.Key); got != "" {
		t.Errorf("no host: human card link = %q, want none", got)
	}
	task := notify.Resource{Key: notify.Key{Type: notify.TypeTask, ID: "tsk_b"}, SpaceID: "spc_a"}
	if got := c.cardAppURL(task.SpaceID, task.Key); got != "" {
		t.Errorf("task card link = %q, want none (only human actions have a page)", got)
	}
}
