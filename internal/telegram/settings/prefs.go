package settings

import (
	"encoding/json"
	"slices"
	"time"
	_ "time/tzdata" // IANA zones are embedded: a user's zone never depends on the host's zone database

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// MaxMutedSpaces bounds a link's muted spaces.
const MaxMutedSpaces = 200

// LinkPrefs are a link's notification preferences as stored in telegram_links.prefs_json: only the fields its user
// changed. Everything else follows the global notification_defaults, so a changed default reaches every link that
// didn't override that field.
type LinkPrefs struct {
	NotificationDefaultsInput
	MutedSpaces *[]string `json:"muted_spaces,omitempty" maxItems:"200" doc:"Spaces whose notifications are never sent to this link"`
}

// EffectivePrefs are what a link notifies about: the global defaults with the link's overrides applied.
type EffectivePrefs struct {
	NotificationDefaults
	MutedSpaces []string `json:"muted_spaces"`
}

// Effective returns the link's preferences under the defaults d.
func (p LinkPrefs) Effective(d NotificationDefaults) EffectivePrefs {
	out := EffectivePrefs{NotificationDefaults: d.Apply(p.NotificationDefaultsInput), MutedSpaces: []string{}}
	if p.MutedSpaces != nil {
		out.MutedSpaces = slices.Clone(*p.MutedSpaces)
	}
	return out
}

// Validate checks the overrides against the defaults d (an override must be a valid value; quiet hours must not
// collapse to nothing once applied).
func (p LinkPrefs) Validate(d NotificationDefaults) error {
	v := &apperr.Validation{}
	p.Effective(d).validate(v, "")
	if p.MutedSpaces != nil && len(*p.MutedSpaces) > MaxMutedSpaces {
		v.Add("muted_spaces", "must list at most %d spaces", MaxMutedSpaces)
	}
	return v.Err()
}

// ParseLinkPrefs decodes stored overrides. Unknown or malformed content yields no overrides, never an error: the link
// then simply follows the defaults.
func ParseLinkPrefs(raw string) LinkPrefs {
	var p LinkPrefs
	if raw == "" || json.Unmarshal([]byte(raw), &p) != nil {
		return LinkPrefs{}
	}
	return p
}

// Marshal encodes the overrides for telegram_links.prefs_json ("{}" when nothing is overridden).
func (p LinkPrefs) Marshal() string {
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// LoadZone resolves a user's IANA time zone. An empty name means the user set none: ok is false, and the digest and
// quiet hours are off for that user (no server-wide fallback, operator answer Q9).
func LoadZone(name string) (loc *time.Location, ok bool) {
	if name == "" || name == "Local" {
		return nil, false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, false
	}
	return loc, true
}

// ValidZone reports whether name is an IANA time zone a user may store ("" = unset is valid).
func ValidZone(name string) bool {
	if name == "" {
		return true
	}
	if len(name) > 64 {
		return false
	}
	_, ok := LoadZone(name)
	return ok
}
