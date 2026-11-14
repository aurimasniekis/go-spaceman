package settings

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
)

func ptr[T any](v T) *T { return &v }

func TestDefaultsAreValidAndMatchThePlan(t *testing.T) {
	t.Parallel()
	d := Defaults()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	// Plan §5.2 and the operator answers Q6-Q9.
	if d.LinkDefaultMaxRole != RoleOperator || d.LinkAllowAdminCap || d.LinkSSOGrants || d.LinkReconfirmDays != 90 || d.LinksPerUser != 1 ||
		d.LinkCodeTTLSeconds != 600 || d.InitDataMaxAgeSeconds != 300 || d.MiniAppSessionIdleSeconds != 900 ||
		d.MiniAppSessionAbsoluteSeconds != 3600 || d.TopicsModeDefault {
		t.Fatalf("link policy defaults %+v", d)
	}
	if d.SendLimits.Sender() != sender.DefaultLimits() {
		t.Fatalf("send limits %+v, want the sender's defaults", d.SendLimits)
	}
	if d.InboundLimits != (InboundLimits{ActionsPerMinute: 30, ActionsBurst: 10, InboxItemsPerHour: 60, DownloadsPerHour: 50, UpdatesPerSecond: 20}) {
		t.Fatalf("inbound limits %+v", d.InboundLimits)
	}
	n := d.NotificationDefaults
	if !n.NewActions || n.MinPriority != "normal" || !n.AssignedToMe || !n.Escalations || !n.Clarifications || !n.OperatorMessages ||
		n.InboxItems != DeliveryDigest || n.TasksInReview != DeliveryDigest || n.FailedRuns != DeliveryDigest || !n.Digest ||
		n.DigestTime != "09:00" || !n.QuietHours || n.QuietHoursStart != "23:00" || n.QuietHoursEnd != "08:00" {
		t.Fatalf("notification defaults %+v", n)
	}
}

func TestResolveSetsOnlyWhatWasSent(t *testing.T) {
	t.Parallel()
	in := Input{LinksPerUser: ptr[int64](2), SendLimits: &SendLimitsInput{ChatBurst: ptr[int64](1)},
		NotificationDefaults: &NotificationDefaultsInput{Digest: ptr(false), DigestTime: ptr("07:30")}}
	s := Resolve(in)
	want := Defaults()
	want.LinksPerUser = 2
	want.SendLimits.ChatBurst = 1
	want.NotificationDefaults.Digest = false
	want.NotificationDefaults.DigestTime = "07:30"
	if s != want {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
	// What is stored is exactly what was sent.
	raw, _ := json.Marshal(in)
	if string(raw) != `{"links_per_user":2,"send_limits":{"chat_burst":1},"notification_defaults":{"digest":false,"digest_time":"07:30"}}` {
		t.Fatalf("stored %s", raw)
	}
	if raw, _ := json.Marshal(Input{}); string(raw) != "{}" {
		t.Fatalf("empty input stored as %s", raw)
	}
}

// Every bounded field refuses values just outside its range, naming the field; values at the bounds pass.
func TestValidateBounds(t *testing.T) {
	t.Parallel()
	type tc struct {
		field string
		set   func(s *Settings, ok bool)
	}
	i64 := func(f func(s *Settings) *int64, lo, hi int64) func(s *Settings, ok bool) {
		return func(s *Settings, ok bool) {
			if ok {
				*f(s) = hi
				return
			}
			*f(s) = lo - 1
		}
	}
	cases := []tc{
		{field: "link_reconfirm_days", set: i64(func(s *Settings) *int64 { return &s.LinkReconfirmDays }, 7, 365)},
		{field: "links_per_user", set: i64(func(s *Settings) *int64 { return &s.LinksPerUser }, 1, 3)},
		{field: "link_code_ttl_seconds", set: i64(func(s *Settings) *int64 { return &s.LinkCodeTTLSeconds }, 120, 1800)},
		{field: "init_data_max_age_seconds", set: i64(func(s *Settings) *int64 { return &s.InitDataMaxAgeSeconds }, 60, 3600)},
		{field: "miniapp_session_idle_seconds", set: i64(func(s *Settings) *int64 { return &s.MiniAppSessionIdleSeconds }, 300, 900)},
		{field: "miniapp_session_absolute_seconds", set: i64(func(s *Settings) *int64 { return &s.MiniAppSessionAbsoluteSeconds }, 900, 43200)},
		{field: "send_limits.chat_burst", set: i64(func(s *Settings) *int64 { return &s.SendLimits.ChatBurst }, 1, 3)},
		{field: "send_limits.group_per_minute", set: i64(func(s *Settings) *int64 { return &s.SendLimits.GroupPerMinute }, 1, 20)},
		{field: "send_limits.queue_total", set: i64(func(s *Settings) *int64 { return &s.SendLimits.QueueTotal }, 1, 50000)},
		{field: "send_limits.queue_per_chat", set: i64(func(s *Settings) *int64 { return &s.SendLimits.QueuePerChat }, 1, 1000)},
		{field: "inbound_limits.actions_per_minute", set: i64(func(s *Settings) *int64 { return &s.InboundLimits.ActionsPerMinute }, 1, 600)},
		{field: "inbound_limits.actions_burst", set: i64(func(s *Settings) *int64 { return &s.InboundLimits.ActionsBurst }, 1, 100)},
		{field: "inbound_limits.inbox_items_per_hour", set: i64(func(s *Settings) *int64 { return &s.InboundLimits.InboxItemsPerHour }, 1, 1000)},
		{field: "inbound_limits.downloads_per_hour", set: i64(func(s *Settings) *int64 { return &s.InboundLimits.DownloadsPerHour }, 1, 1000)},
		{field: "inbound_limits.updates_per_second", set: i64(func(s *Settings) *int64 { return &s.InboundLimits.UpdatesPerSecond }, 1, 100)},
		{field: "send_limits.global_per_second", set: func(s *Settings, ok bool) {
			s.SendLimits.GlobalPerSecond = map[bool]float64{true: 30, false: 30.5}[ok]
		}},
		{field: "send_limits.chat_per_second", set: func(s *Settings, ok bool) {
			s.SendLimits.ChatPerSecond = map[bool]float64{true: 0.25, false: 0}[ok]
		}},
		{field: "link_default_max_role", set: func(s *Settings, ok bool) {
			s.LinkDefaultMaxRole = map[bool]string{true: RoleViewer, false: "admin"}[ok]
		}},
		{field: "notification_defaults.min_priority", set: func(s *Settings, ok bool) {
			s.NotificationDefaults.MinPriority = map[bool]string{true: "urgent", false: "critical"}[ok]
		}},
		{field: "notification_defaults.reminders", set: func(s *Settings, ok bool) {
			s.NotificationDefaults.Reminders = map[bool]string{true: DeliveryOff, false: "email"}[ok]
		}},
		{field: "notification_defaults.failed_runs", set: func(s *Settings, ok bool) {
			s.NotificationDefaults.FailedRuns = map[bool]string{true: DeliveryPush, false: ""}[ok]
		}},
		{field: "notification_defaults.digest_time", set: func(s *Settings, ok bool) {
			s.NotificationDefaults.DigestTime = map[bool]string{true: "23:59", false: "24:00"}[ok]
		}},
		{field: "notification_defaults.quiet_hours_start", set: func(s *Settings, ok bool) {
			s.NotificationDefaults.QuietHoursStart = map[bool]string{true: "00:00", false: "9:00"}[ok]
		}},
		{field: "notification_defaults.quiet_hours_end", set: func(s *Settings, ok bool) {
			s.NotificationDefaults.QuietHoursEnd = map[bool]string{true: "07:00", false: "23:00"}[ok] // equal to the start
		}},
	}
	for _, c := range cases {
		ok := Defaults()
		c.set(&ok, true)
		if err := ok.Validate(); err != nil {
			t.Errorf("%s at its bound: %v", c.field, err)
		}
		bad := Defaults()
		c.set(&bad, false)
		err := bad.Validate()
		var ae *apperr.Error
		if err == nil || !errors.As(err, &ae) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s out of range: %v", c.field, err)
		}
	}
	// Cross-field rules.
	s := Defaults()
	s.MiniAppSessionIdleSeconds, s.MiniAppSessionAbsoluteSeconds = 1800, 900
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "miniapp_session_absolute_seconds") {
		t.Errorf("absolute below idle: %v", err)
	}
	s = Defaults()
	s.SendLimits.QueueTotal, s.SendLimits.QueuePerChat = 10, 20
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "queue_per_chat") {
		t.Errorf("per-chat queue above the total: %v", err)
	}
}

// The settings' bounds are the sender's: anything Validate accepts, the sender accepts.
func TestSendBoundsMatchTheSender(t *testing.T) {
	t.Parallel()
	s := Defaults()
	s.SendLimits = SendLimits{GlobalPerSecond: sender.MaxGlobalPerSecond, ChatPerSecond: sender.MaxChatPerSecond, ChatBurst: sender.MaxChatBurst,
		GroupPerMinute: sender.MaxGroupPerMinute, QueueTotal: sender.MaxQueueTotal, QueuePerChat: sender.MaxQueuePerChat}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := s.SendLimits.Sender().Validate(); err != nil {
		t.Fatal(err)
	}
}

// Every field of Settings has a matching Input field (same JSON name), so every setting can be set through the API and
// so by the Terraform provider.
func TestEveryFieldIsSettable(t *testing.T) {
	t.Parallel()
	pairs := [][2]reflect.Type{
		{reflect.TypeFor[Settings](), reflect.TypeFor[Input]()},
		{reflect.TypeFor[SendLimits](), reflect.TypeFor[SendLimitsInput]()},
		{reflect.TypeFor[InboundLimits](), reflect.TypeFor[InboundLimitsInput]()},
		{reflect.TypeFor[NotificationDefaults](), reflect.TypeFor[NotificationDefaultsInput]()},
	}
	name := func(f reflect.StructField) string { return strings.Split(f.Tag.Get("json"), ",")[0] }
	for _, p := range pairs {
		in := map[string]bool{}
		for i := range p[1].NumField() {
			f := p[1].Field(i)
			if f.Type.Kind() != reflect.Pointer {
				t.Errorf("%s.%s is not optional", p[1].Name(), f.Name)
			}
			in[name(f)] = true
		}
		for i := range p[0].NumField() {
			if f := p[0].Field(i); !in[name(f)] {
				t.Errorf("%s.%s (%s) has no input field", p[0].Name(), f.Name, name(f))
			}
		}
		if p[0].NumField() != p[1].NumField() {
			t.Errorf("%s has %d fields, its input %d", p[0].Name(), p[0].NumField(), p[1].NumField())
		}
	}
}

func TestParseClock(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]int{"00:00": 0, "09:00": 540, "23:59": 1439} {
		if got, err := ParseClock(s); err != nil || got != want {
			t.Errorf("%s: %d %v", s, got, err)
		}
	}
	for _, s := range []string{"", "9:00", "24:00", "12:60", "12:00:00", " 12:00"} {
		if _, err := ParseClock(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestSpaceInput(t *testing.T) {
	t.Parallel()
	s, err := SpaceInput{}.apply("spc_1")
	if err != nil || s != DefaultSpace("spc_1") || s.Visible() {
		t.Fatalf("defaults %+v %v", s, err)
	}
	if s.ContentLevel != ContentFull || s.EnforcedApprovals != ApprovalsWebOnly || s.Topics || s.TerminalWatch {
		t.Fatalf("defaults %+v", s)
	}
	s, err = SpaceInput{Enabled: ptr(true), ContentLevel: ptr(ContentOff), EnforcedApprovals: ptr(ApprovalsConfirm), Topics: ptr(true),
		TerminalWatch: ptr(true)}.apply("spc_1")
	if err != nil || s.Visible() || s.ContentLevel != ContentOff || s.EnforcedApprovals != ApprovalsConfirm || !s.Topics || !s.TerminalWatch {
		t.Fatalf("set %+v %v (content level off hides the space)", s, err)
	}
	for _, level := range []string{ContentFull, ContentSummary} {
		if s, _ := (SpaceInput{Enabled: ptr(true), ContentLevel: ptr(level)}).apply("spc_1"); !s.Visible() {
			t.Errorf("enabled at %s is hidden", level)
		}
	}
	if _, err := (SpaceInput{ContentLevel: ptr("bodies")}).apply("spc_1"); err == nil || !strings.Contains(err.Error(), "content_level") {
		t.Fatalf("bad content level: %v", err)
	}
	if _, err := (SpaceInput{EnforcedApprovals: ptr("chat")}).apply("spc_1"); err == nil || !strings.Contains(err.Error(), "enforced_approvals") {
		t.Fatalf("bad approvals: %v", err)
	}
}

// A link stores only its user's overrides: a changed default reaches a link that didn't override the field, and not one
// that did (plan §5.3).
func TestLinkPrefsFollowDefaultsUnlessOverridden(t *testing.T) {
	t.Parallel()
	d := Defaults().NotificationDefaults
	plain := LinkPrefs{}
	own := LinkPrefs{NotificationDefaultsInput: NotificationDefaultsInput{DigestTime: ptr("07:00"), InboxItems: ptr(DeliveryPush)},
		MutedSpaces: &[]string{"spc_x"}}
	if got := plain.Marshal(); got != "{}" {
		t.Fatalf("no overrides stored as %s", got)
	}
	if got := own.Marshal(); got != `{"inbox_items":"push","digest_time":"07:00","muted_spaces":["spc_x"]}` {
		t.Fatalf("overrides stored as %s", got)
	}
	if e := plain.Effective(d); e.DigestTime != "09:00" || e.InboxItems != DeliveryDigest || len(e.MutedSpaces) != 0 || e.MutedSpaces == nil {
		t.Fatalf("plain %+v", e)
	}
	d.DigestTime, d.InboxItems, d.FailedRuns = "10:00", DeliveryOff, DeliveryPush
	if e := plain.Effective(d); e.DigestTime != "10:00" || e.InboxItems != DeliveryOff || e.FailedRuns != DeliveryPush {
		t.Fatalf("plain after a default change %+v", e)
	}
	if e := own.Effective(d); e.DigestTime != "07:00" || e.InboxItems != DeliveryPush || e.FailedRuns != DeliveryPush ||
		!reflect.DeepEqual(e.MutedSpaces, []string{"spc_x"}) {
		t.Fatalf("own after a default change %+v", e)
	}
	// Round trip through storage.
	if back := ParseLinkPrefs(own.Marshal()); !reflect.DeepEqual(back, own) {
		t.Fatalf("round trip %+v", back)
	}
	if back := ParseLinkPrefs("not json"); !reflect.DeepEqual(back, LinkPrefs{}) {
		t.Fatalf("malformed %+v", back)
	}
	// Validation applies the overrides to the defaults.
	if err := own.Validate(d); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []LinkPrefs{
		{NotificationDefaultsInput: NotificationDefaultsInput{Reminders: ptr("loud")}},
		{NotificationDefaultsInput: NotificationDefaultsInput{QuietHoursStart: ptr("08:00")}}, // equals the default end
		{MutedSpaces: ptr(make([]string, MaxMutedSpaces+1))},
	} {
		if err := bad.Validate(d); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestZones(t *testing.T) {
	t.Parallel()
	for _, z := range []string{"", "UTC", "Europe/Vilnius", "America/New_York", "Asia/Kolkata"} {
		if !ValidZone(z) {
			t.Errorf("%q refused", z)
		}
	}
	for _, z := range []string{"Local", "Mars/Olympus", "../etc/passwd", "Europe/Vilnius ", strings.Repeat("A", 65)} {
		if ValidZone(z) {
			t.Errorf("%q accepted", z)
		}
	}
	if _, ok := LoadZone(""); ok {
		t.Error("an unset zone loaded: there is no fallback")
	}
	if loc, ok := LoadZone("Europe/Vilnius"); !ok || loc.String() != "Europe/Vilnius" {
		t.Errorf("Europe/Vilnius: %v %v", loc, ok)
	}
}
