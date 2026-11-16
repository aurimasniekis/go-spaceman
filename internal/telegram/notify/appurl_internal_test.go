package notify

import "testing"

func TestNotifierAppURLIsPerCard(t *testing.T) {
	// Without the option a card offers no continuation button.
	if got := (&Notifier{}).appURL(&Resource{Key: Key{Type: TypeHumanAction, ID: "hum_1"}}); got != "" {
		t.Errorf("unset AppURL: got %q, want \"\"", got)
	}
	var seen []Resource
	n := &Notifier{o: Options{AppURL: func(r Resource) string {
		seen = append(seen, r)
		if r.Type != TypeHumanAction {
			return ""
		}
		return "https://mini.example/spaces/" + r.SpaceID + "/human?action=" + r.ID
	}}}
	if got, want := n.appURL(&Resource{Key: Key{Type: TypeHumanAction, ID: "hum_1"}, SpaceID: "spc_a"}), "https://mini.example/spaces/spc_a/human?action=hum_1"; got != want {
		t.Errorf("human card: got %q, want %q", got, want)
	}
	if got := n.appURL(&Resource{Key: Key{Type: TypeTask, ID: "tsk_1"}, SpaceID: "spc_a"}); got != "" {
		t.Errorf("task card: got %q, want \"\"", got)
	}
	if len(seen) != 2 || seen[0].ID != "hum_1" || seen[1].ID != "tsk_1" {
		t.Errorf("the function got the cards %+v, want each card in turn", seen)
	}
}
