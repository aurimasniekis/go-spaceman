package settings

import (
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// Content levels of a space (plan §2.0, Q1).
const (
	ContentFull    = "full"
	ContentSummary = "summary"
	ContentOff     = "off"
)

// Enforced-approval modes of a space (plan §2.4, Q2).
const (
	ApprovalsWebOnly = "web_only"
	ApprovalsConfirm = "confirm"
)

// Space is a space's Telegram settings. A space without stored settings, or with enabled false, is invisible to
// Telegram (opt-in per space, Q1).
type Space struct {
	SpaceID           string     `json:"space_id"`
	Stored            bool       `json:"stored" doc:"false: the defaults (Telegram off for the space)"`
	Enabled           bool       `json:"enabled" doc:"The space is visible through Telegram"`
	ContentLevel      string     `json:"content_level" enum:"full,summary,off" doc:"full: bodies in chat; summary: bodies only in the Mini App; off: the space stays hidden"`
	EnforcedApprovals string     `json:"enforced_approvals" enum:"web_only,confirm" doc:"Whether enforced approvals can be decided in chat with a second confirmation, or only on the web"`
	Topics            bool       `json:"topics" doc:"Deprecated, no effect (ADR-0089): the chat layout is chosen per link (notification_defaults.threads)"`
	TerminalWatch     bool       `json:"terminal_watch" doc:"Run terminals can be watched (read-only) from the Mini App"`
	CreatedAt         *time.Time `json:"created_at,omitempty"`
	CreatedBy         string     `json:"created_by,omitempty"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
	UpdatedBy         string     `json:"updated_by,omitempty"`
	Version           int64      `json:"version"`

	// GroupChatID is the forum supergroup bound to the space (group mode, ADR-0083 amendment TG-13; 0: none), read
	// from space_telegram_settings.group_chat_id.
	GroupChatID int64 `json:"group_chat_id,omitempty" doc:"The forum supergroup bound to the space (group mode); omitted when none. Members of the group who linked their account can use the space's summaries there"`
}

// DefaultSpace is a space's settings before anyone set them.
func DefaultSpace(spaceID string) Space {
	return Space{SpaceID: spaceID, ContentLevel: ContentFull, EnforcedApprovals: ApprovalsWebOnly}
}

// Visible reports whether the space can be seen through Telegram at all: enabled, with a content level other than off
// (ADR-0083 decision 13: off hides the space).
func (s Space) Visible() bool { return s.Enabled && s.ContentLevel != ContentOff }

// IsGroupChat reports whether a chat ID is anything but a private chat with a person. Telegram gives a private chat the
// person's user ID (positive) and groups, supergroups and channels negative IDs; zero (unknown) counts as a group, so
// the summary rule below fails closed.
func IsGroupChat(chatID int64) bool { return chatID <= 0 }

// Full reports whether bodies of the space may be sent: only when its content level is full AND the message isn't for a
// group chat. Anything posted to a group chat is summary whatever the space says (ADR-0083 amendment TG-13). Every
// Telegram renderer asks this, never the content level itself (TestContentLevelOnlyThroughFull).
func Full(s Space, inGroup bool) bool {
	return s.ContentLevel == ContentFull && !inGroup
}

// SpaceInput sets a space's Telegram settings. A field left out takes its default.
type SpaceInput struct {
	Enabled           *bool   `json:"enabled,omitempty" doc:"Default false"`
	ContentLevel      *string `json:"content_level,omitempty" enum:"full,summary,off" doc:"Default full (bodies are only sent once the space is enabled)"`
	EnforcedApprovals *string `json:"enforced_approvals,omitempty" enum:"web_only,confirm" doc:"Default web_only"`
	Topics            *bool   `json:"topics,omitempty" doc:"Deprecated, no effect (the chat layout is per link). Default false"`
	TerminalWatch     *bool   `json:"terminal_watch,omitempty" doc:"Default false"`
	GroupChatID       *int64  `json:"group_chat_id,omitempty" doc:"Binds the forum supergroup (its chat ID, negative; the bot must be a member) to the space; 0 unbinds it. Left out, the current binding stays. Changing it needs telegram_group:write (admins); 409 when the group is bound to another space"`
}

// apply returns the defaults for spaceID with every field of in set, validated.
func (in SpaceInput) apply(spaceID string) (Space, error) {
	s := DefaultSpace(spaceID)
	set(&s.Enabled, in.Enabled)
	set(&s.ContentLevel, in.ContentLevel)
	set(&s.EnforcedApprovals, in.EnforcedApprovals)
	set(&s.Topics, in.Topics)
	set(&s.TerminalWatch, in.TerminalWatch)
	v := &apperr.Validation{}
	switch s.ContentLevel {
	case ContentFull, ContentSummary, ContentOff:
	default:
		v.Add("content_level", "must be full, summary or off")
	}
	switch s.EnforcedApprovals {
	case ApprovalsWebOnly, ApprovalsConfirm:
	default:
		v.Add("enforced_approvals", "must be web_only or confirm")
	}
	return s, v.Err()
}
