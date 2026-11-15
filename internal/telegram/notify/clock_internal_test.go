package notify

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, ok := settings.LoadZone(name)
	if !ok {
		t.Fatalf("zone %q", name)
	}
	return loc
}

func prefs(start, end string) settings.EffectivePrefs {
	d := settings.Defaults().NotificationDefaults
	d.QuietHours, d.QuietHoursStart, d.QuietHoursEnd = true, start, end
	return settings.LinkPrefs{}.Effective(d)
}

// Quiet hours follow the wall clock of the user's zone, across midnight, at the exact boundaries (the start is
// quiet, the end is not), and never without a zone.
func TestQuietHoursBoundariesAcrossZones(t *testing.T) {
	utc := func(h, m, s int) time.Time { return time.Date(2026, 11, 14, h, m, s, 0, time.UTC) }
	cases := []struct {
		name       string
		zone       string
		start, end string
		at         time.Time
		want       bool
	}{
		{"Vilnius 22:59:59 local", "Europe/Vilnius", "23:00", "08:00", utc(20, 59, 59), false},
		{"Vilnius 23:00 local starts", "Europe/Vilnius", "23:00", "08:00", utc(21, 0, 0), true},
		{"Vilnius past midnight", "Europe/Vilnius", "23:00", "08:00", utc(23, 30, 0), true},
		{"Vilnius 07:59:59 local", "Europe/Vilnius", "23:00", "08:00", utc(5, 59, 59), true},
		{"Vilnius 08:00 local ends", "Europe/Vilnius", "23:00", "08:00", utc(6, 0, 0), false},
		{"New York is still afternoon at 21:00Z", "America/New_York", "23:00", "08:00", utc(21, 0, 0), false},
		{"New York 23:00 local is 04:00Z", "America/New_York", "23:00", "08:00", utc(4, 0, 0).Add(24 * time.Hour), true},
		{"Kolkata (+5:30) 23:00 local is 17:30Z", "Asia/Kolkata", "23:00", "08:00", utc(17, 30, 0), true},
		{"Kolkata 22:59 local", "Asia/Kolkata", "23:00", "08:00", utc(17, 29, 0), false},
		{"Auckland (+13 in November) 08:00 local is 19:00Z", "Pacific/Auckland", "23:00", "08:00", utc(19, 0, 0), false},
		{"Auckland 07:59 local", "Pacific/Auckland", "23:00", "08:00", utc(18, 59, 0), true},
		{"a window inside one day", "Europe/Vilnius", "13:00", "14:00", utc(11, 30, 0), true},
		{"the same window, start minute", "Europe/Vilnius", "13:00", "14:00", utc(11, 0, 0), true},
		{"the same window, a minute before", "Europe/Vilnius", "13:00", "14:00", utc(10, 59, 0), false},
		{"the same window, end", "Europe/Vilnius", "13:00", "14:00", utc(12, 0, 0), false},
	}
	for _, c := range cases {
		if got := quiet(prefs(c.start, c.end), zone(t, c.zone), c.at); got != c.want {
			t.Errorf("%s: quiet = %v, want %v", c.name, got, c.want)
		}
	}
	if quiet(prefs("23:00", "08:00"), nil, utc(23, 0, 0)) {
		t.Error("no zone: quiet hours are off")
	}
	p := prefs("23:00", "08:00")
	p.QuietHours = false
	if quiet(p, zone(t, "UTC"), utc(23, 30, 0)) {
		t.Error("quiet hours turned off")
	}
	if quiet(prefs("10:00", "10:00"), zone(t, "UTC"), utc(10, 0, 0)) {
		t.Error("an empty window is never quiet")
	}
}

// Quiet hours move with the daylight-saving clock: 23:00 in New York is 03:00Z in summer and 04:00Z in winter.
func TestQuietHoursFollowDaylightSaving(t *testing.T) {
	ny := zone(t, "America/New_York")
	p := prefs("23:00", "08:00")
	summer := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC) // 23:00 EDT
	winter := time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC) // 22:00 EST
	if !quiet(p, ny, summer) || quiet(p, ny, winter) {
		t.Errorf("summer %v, winter %v: want true, false", quiet(p, ny, summer), quiet(p, ny, winter))
	}
}

func TestNextClock(t *testing.T) {
	vil, ny := zone(t, "Europe/Vilnius"), zone(t, "America/New_York")
	at := func(y int, m time.Month, d, h, mi int, loc *time.Location) time.Time {
		return time.Date(y, m, d, h, mi, 0, 0, loc)
	}
	cases := []struct {
		name  string
		loc   *time.Location
		clock string
		after time.Time
		want  time.Time
	}{
		{"later today in Vilnius", vil, "09:00", at(2026, 11, 14, 8, 0, vil), at(2026, 11, 14, 9, 0, vil)},
		{"exactly now is not after: tomorrow", vil, "09:00", at(2026, 11, 14, 9, 0, vil), at(2026, 11, 15, 9, 0, vil)},
		{"07:00Z is 09:00 in Vilnius", vil, "09:00", time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC), at(2026, 11, 15, 9, 0, vil)},
		{"the same instant in New York is 02:00 local", ny, "09:00", time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC), at(2026, 11, 14, 9, 0, ny)},
		// 01:30 exists twice on 2026-11-01 (EDT then EST): the first one counts, and the next day follows.
		{"fall back twice", ny, "01:30", at(2026, 11, 1, 0, 0, ny), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		{"after the first 01:30 the next is tomorrow", ny, "01:30", time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), at(2026, 11, 2, 1, 30, ny)},
	}
	for _, c := range cases {
		got, ok := nextClock(c.loc, c.clock, c.after)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%s: got %v (%v), want %v", c.name, got, ok, c.want)
		}
	}
	// 02:30 does not exist on 2026-03-08 in New York (02:00 jumps to 03:00). Go resolves it to one instant of that day;
	// what matters is that it is a single instant that day, and the next digest is the next day's.
	gap, ok := nextClock(ny, "02:30", at(2026, 3, 8, 0, 0, ny))
	if !ok || gap.In(ny).Day() != 8 {
		t.Fatalf("gap day: %v", gap)
	}
	if nxt, _ := nextClock(ny, "02:30", gap); nxt.In(ny).Day() != 9 || nxt.In(ny).Hour() != 2 {
		t.Errorf("after the gap day's digest the next is %v, want 02:30 on the 9th", nxt.In(ny))
	}
	if _, ok := nextClock(vil, "25:99", time.Now()); ok {
		t.Error("a bad clock time has no next instant")
	}
}

// A card's baseline lives in base only while its first send is queued: record seeds byCard from it once, later
// records keep what byCard holds, and the last send's Done leaves nothing behind.
func TestCardsBaselineIsSeededOnceAndCleared(t *testing.T) {
	c := newCards(nil, 10, time.Now)
	k := Key{Type: TypeHumanAction, ID: "hum_1"}
	r := Resource{Key: k, SpaceID: "spc_1", Version: 1, Kind: KindAction}
	d1, d2 := c.queue(1, &r), c.queue(1, &r)
	m := Mapping{ChatID: 1, MessageID: 7, SpaceID: "spc_1", Key: k, Version: 1}
	c.record(m, &r)
	d1(nil)
	r2 := r
	r2.Version, r2.Assignee = 2, "usr_1"
	if _, _, reassigned := c.observe(1, &r2); !reassigned {
		t.Fatal("the assignment was not seen")
	}
	m.Version = 2
	c.record(m, &r2)
	d2(nil)
	if _, _, reassigned := c.observe(1, &r2); reassigned {
		t.Error("the same assignment is seen again")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.base) != 0 || len(c.queued) != 0 {
		t.Errorf("base %d, queued %d left after the last send", len(c.base), len(c.queued))
	}
}
