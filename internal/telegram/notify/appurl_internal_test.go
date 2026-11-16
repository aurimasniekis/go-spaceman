package notify

import "testing"

func TestNotifierItemURLIsPerCard(t *testing.T) {
	// Without the options a card offers no Open in app button.
	if got := (&Notifier{}).itemURL("spc_a", Key{Type: TypeHumanAction, ID: "hum_1"}); got != "" {
		t.Errorf("unset ItemURL: got %q, want \"\"", got)
	}
	type call struct{ space, id string }
	var seen []call
	n := &Notifier{o: Options{ItemURL: func(spaceID string, k Key) string {
		seen = append(seen, call{spaceID, k.ID})
		if k.Type != TypeHumanAction {
			return ""
		}
		return "https://mini.example/spaces/" + spaceID + "/human?action=" + k.ID
	}}}
	if got, want := n.itemURL("spc_a", Key{Type: TypeHumanAction, ID: "hum_1"}), "https://mini.example/spaces/spc_a/human?action=hum_1"; got != want {
		t.Errorf("human card: got %q, want %q", got, want)
	}
	if got := n.itemURL("spc_a", Key{Type: TypeTask, ID: "tsk_1"}); got != "" {
		t.Errorf("task card: got %q, want \"\"", got)
	}
	if len(seen) != 2 || seen[0].id != "hum_1" || seen[1].id != "tsk_1" || seen[0].space != "spc_a" {
		t.Errorf("the function got the cards %+v, want each card in turn", seen)
	}
}
