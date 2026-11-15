// Package settings keeps the Telegram bot's policy and defaults (the telegram_settings singleton) and each space's
// Telegram settings (space_telegram_settings), ADR-0083 and plan §5.2 (TG-3c).
//
// Every setting has an API operation and a Terraform attribute (terraform-provider-spaceman `spaceman_telegram_settings`
// and `spaceman_space_telegram`). The global settings are stored as the fields the administrator set, nothing else: a
// field left out takes its built-in default, so a missing row, or an empty one, is the defaults. The API always shows
// the effective values.
//
// Readers use the Cache, which keeps both in memory and is refreshed over the in-process event bus after every
// committed change, so a change reaches the outbound sender and the inbound limiter without a restart and no update
// reads the database for its policy.
package settings

import (
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
)

// Link caps a link may get by default (plan §3.1, Q8). The admin cap is never a default: it needs link_allow_admin_cap
// and is chosen per link.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
)

// Delivery choices of a notification kind.
const (
	DeliveryPush   = "push"
	DeliveryDigest = "digest"
	DeliveryOff    = "off"
)

// Bounds of the link policy and lifetimes (plan §5.2).
const (
	MinReconfirmDays, MaxReconfirmDays           = 7, 365
	MinLinksPerUser, MaxLinksPerUser             = 1, 3
	MinLinkCodeTTL, MaxLinkCodeTTL               = 120, 1800
	MinInitDataMaxAge, MaxInitDataMaxAge         = 60, 3600
	MinMiniAppIdle, MaxMiniAppIdle               = 300, 3600
	MinMiniAppAbsolute, MaxMiniAppAbsolute       = 900, 43200
	MaxActionsPerMinute, MaxActionsBurst         = 600, 100
	MaxInboxItemsPerHour, MaxDownloadsPerHour    = 1000, 1000
	MaxUpdatesPerSecond                          = 100
	defaultReconfirmDays, defaultLinksPerUser    = 90, 1
	defaultLinkCodeTTL, defaultInitDataMaxAge    = 600, 300
	defaultMiniAppIdle, defaultMiniAppAbsolute   = 900, 3600
	defaultActionsPerMinute, defaultActionsBurst = 30, 10
)

// Settings are the effective global Telegram settings: the built-in defaults with the stored fields applied.
type Settings struct {
	LinkDefaultMaxRole            string               `json:"link_default_max_role" enum:"viewer,operator" doc:"The cap a new link gets: its Telegram session never has more than this role in any space, whatever the user's grants"`
	LinkAllowAdminCap             bool                 `json:"link_allow_admin_cap" doc:"Whether a link may be given the admin cap at all"`
	LinkSSOGrants                 bool                 `json:"link_sso_grants" doc:"Whether a link confirmed from an SSO session also carries the user's SSO-mapped grants, until that session's absolute expiry"`
	LinkReconfirmDays             int64                `json:"link_reconfirm_days" doc:"Days after which a link must be confirmed again from the web (7-365)"`
	LinksPerUser                  int64                `json:"links_per_user" doc:"Telegram accounts one SpaceMan user may link (1-3)"`
	LinkCodeTTLSeconds            int64                `json:"link_code_ttl_seconds" doc:"Lifetime of a link code (120-1800 s)"`
	InitDataMaxAgeSeconds         int64                `json:"init_data_max_age_seconds" doc:"Oldest Mini App initData accepted for a session exchange (60-3600 s)"`
	MiniAppSessionIdleSeconds     int64                `json:"miniapp_session_idle_seconds" doc:"A Mini App session ends after this long without a request (300-3600 s)"`
	MiniAppSessionAbsoluteSeconds int64                `json:"miniapp_session_absolute_seconds" doc:"A Mini App session ends this long after the exchange, whatever its use (900-43200 s, at least the idle timeout)"`
	SendLimits                    SendLimits           `json:"send_limits" doc:"Outbound rate limits and queue bounds"`
	InboundLimits                 InboundLimits        `json:"inbound_limits" doc:"Inbound abuse limits"`
	NotificationDefaults          NotificationDefaults `json:"notification_defaults" doc:"Each link's notification preferences, unless its user changed them"`
	TopicsModeDefault             bool                 `json:"topics_mode_default" doc:"Whether a new link starts in topics mode (a topic per space in the private chat)"`
}

// SendLimits bound what the bot sends (plan §4.4), at or below Telegram's published limits.
type SendLimits struct {
	GlobalPerSecond float64 `json:"global_per_second" doc:"Messages per second over every chat (above 0, at most 30)"`
	ChatPerSecond   float64 `json:"chat_per_second" doc:"Messages per second to one private chat (above 0, at most 1)"`
	ChatBurst       int64   `json:"chat_burst" doc:"Messages one private chat may get in a burst (1-3)"`
	GroupPerMinute  int64   `json:"group_per_minute" doc:"Messages per minute to one group (1-20)"`
	QueueTotal      int64   `json:"queue_total" doc:"Messages waiting over every chat (1-50000); beyond it the least urgent are dropped"`
	QueuePerChat    int64   `json:"queue_per_chat" doc:"Messages waiting for one chat (1-1000, at most queue_total); beyond it the chat gets one \"N updates\" notice"`
}

// InboundLimits bound what linked users can make central do (plan §3.5). Excess gets a single "slow down" answer.
type InboundLimits struct {
	ActionsPerMinute  int64 `json:"actions_per_minute" doc:"Updates (messages and button taps) one link may send per minute (1-600)"`
	ActionsBurst      int64 `json:"actions_burst" doc:"Updates one link may send in a burst (1-100)"`
	InboxItemsPerHour int64 `json:"inbox_items_per_hour" doc:"Inbox items one link may create per hour (1-1000)"`
	DownloadsPerHour  int64 `json:"downloads_per_hour" doc:"Attachment downloads one link may make per hour (1-1000)"`
	UpdatesPerSecond  int64 `json:"updates_per_second" doc:"Updates central handles per second over every chat (1-100)"`
}

// NotificationDefaults are what a link notifies about until its user overrides a field (plan §2.5, Q9). Digest and
// quiet-hour times are wall-clock times in each user's own time zone; a user without one gets neither.
type NotificationDefaults struct {
	NewActions       bool   `json:"new_actions" doc:"New human actions in spaces the link can read"`
	MinPriority      string `json:"min_priority" enum:"low,normal,high,urgent" doc:"Only new actions of at least this priority (assignments always count)"`
	AssignedToMe     bool   `json:"assigned_to_me" doc:"An action was assigned to the user"`
	Escalations      bool   `json:"escalations" doc:"An action escalated"`
	Reminders        string `json:"reminders" enum:"push,digest,off" doc:"Reminders of pending actions"`
	Clarifications   bool   `json:"clarifications" doc:"Clarification requests and replies"`
	OperatorMessages bool   `json:"operator_messages" doc:"Agent messages to operators"`
	InboxItems       string `json:"inbox_items" enum:"push,digest,off" doc:"New inbox items"`
	TasksInReview    string `json:"tasks_in_review" enum:"push,digest,off" doc:"Tasks entering review"`
	FailedRuns       string `json:"failed_runs" enum:"push,digest,off" doc:"Failed or lost runs"`
	Digest           bool   `json:"digest" doc:"Send a daily digest"`
	DigestTime       string `json:"digest_time" doc:"When the digest is sent, as HH:MM in the user's time zone"`
	QuietHours       bool   `json:"quiet_hours" doc:"Send silently (no sound) during quiet hours"`
	QuietHoursStart  string `json:"quiet_hours_start" doc:"Quiet hours start, as HH:MM in the user's time zone"`
	QuietHoursEnd    string `json:"quiet_hours_end" doc:"Quiet hours end, as HH:MM in the user's time zone (before the start: they run past midnight)"`
}

// Defaults are the built-in settings (plan §5.2, operator answers Q6–Q9).
func Defaults() Settings {
	sl := sender.DefaultLimits()
	return Settings{
		LinkDefaultMaxRole: RoleOperator, LinkReconfirmDays: defaultReconfirmDays, LinksPerUser: defaultLinksPerUser,
		LinkCodeTTLSeconds: defaultLinkCodeTTL, InitDataMaxAgeSeconds: defaultInitDataMaxAge,
		MiniAppSessionIdleSeconds: defaultMiniAppIdle, MiniAppSessionAbsoluteSeconds: defaultMiniAppAbsolute,
		SendLimits: SendLimits{GlobalPerSecond: sl.GlobalPerSecond, ChatPerSecond: sl.ChatPerSecond, ChatBurst: int64(sl.ChatBurst),
			GroupPerMinute: int64(sl.GroupPerMinute), QueueTotal: int64(sl.QueueTotal), QueuePerChat: int64(sl.QueuePerChat)},
		InboundLimits: InboundLimits{ActionsPerMinute: defaultActionsPerMinute, ActionsBurst: defaultActionsBurst, InboxItemsPerHour: 60,
			DownloadsPerHour: 50, UpdatesPerSecond: 20},
		NotificationDefaults: NotificationDefaults{NewActions: true, MinPriority: "normal", AssignedToMe: true, Escalations: true,
			Reminders: DeliveryPush, Clarifications: true, OperatorMessages: true, InboxItems: DeliveryDigest, TasksInReview: DeliveryDigest,
			FailedRuns: DeliveryDigest, Digest: true, DigestTime: "09:00", QuietHours: true, QuietHoursStart: "23:00", QuietHoursEnd: "08:00"},
	}
}

// Input sets the global settings. A field left out (or null) takes its built-in default; only the fields sent are
// stored, so a later change of a default reaches every field nobody set.
type Input struct {
	LinkDefaultMaxRole            *string                    `json:"link_default_max_role,omitempty" enum:"viewer,operator" doc:"Default operator"`
	LinkAllowAdminCap             *bool                      `json:"link_allow_admin_cap,omitempty" doc:"Default false"`
	LinkSSOGrants                 *bool                      `json:"link_sso_grants,omitempty" doc:"Default false"`
	LinkReconfirmDays             *int64                     `json:"link_reconfirm_days,omitempty" minimum:"7" maximum:"365" doc:"Default 90"`
	LinksPerUser                  *int64                     `json:"links_per_user,omitempty" minimum:"1" maximum:"3" doc:"Default 1"`
	LinkCodeTTLSeconds            *int64                     `json:"link_code_ttl_seconds,omitempty" minimum:"120" maximum:"1800" doc:"Default 600"`
	InitDataMaxAgeSeconds         *int64                     `json:"init_data_max_age_seconds,omitempty" minimum:"60" maximum:"3600" doc:"Default 300"`
	MiniAppSessionIdleSeconds     *int64                     `json:"miniapp_session_idle_seconds,omitempty" minimum:"300" maximum:"3600" doc:"Default 900"`
	MiniAppSessionAbsoluteSeconds *int64                     `json:"miniapp_session_absolute_seconds,omitempty" minimum:"900" maximum:"43200" doc:"Default 3600; at least the idle timeout"`
	SendLimits                    *SendLimitsInput           `json:"send_limits,omitempty"`
	InboundLimits                 *InboundLimitsInput        `json:"inbound_limits,omitempty"`
	NotificationDefaults          *NotificationDefaultsInput `json:"notification_defaults,omitempty"`
	TopicsModeDefault             *bool                      `json:"topics_mode_default,omitempty" doc:"Default false"`
}

// SendLimitsInput sets send_limits; a field left out takes its default.
type SendLimitsInput struct {
	GlobalPerSecond *float64 `json:"global_per_second,omitempty" exclusiveMinimum:"0" maximum:"30" doc:"Default 25"`
	ChatPerSecond   *float64 `json:"chat_per_second,omitempty" exclusiveMinimum:"0" maximum:"1" doc:"Default 1"`
	ChatBurst       *int64   `json:"chat_burst,omitempty" minimum:"1" maximum:"3" doc:"Default 3"`
	GroupPerMinute  *int64   `json:"group_per_minute,omitempty" minimum:"1" maximum:"20" doc:"Default 20"`
	QueueTotal      *int64   `json:"queue_total,omitempty" minimum:"1" maximum:"50000" doc:"Default 5000"`
	QueuePerChat    *int64   `json:"queue_per_chat,omitempty" minimum:"1" maximum:"1000" doc:"Default 50; at most queue_total"`
}

// InboundLimitsInput sets inbound_limits; a field left out takes its default.
type InboundLimitsInput struct {
	ActionsPerMinute  *int64 `json:"actions_per_minute,omitempty" minimum:"1" maximum:"600" doc:"Default 30"`
	ActionsBurst      *int64 `json:"actions_burst,omitempty" minimum:"1" maximum:"100" doc:"Default 10"`
	InboxItemsPerHour *int64 `json:"inbox_items_per_hour,omitempty" minimum:"1" maximum:"1000" doc:"Default 60"`
	DownloadsPerHour  *int64 `json:"downloads_per_hour,omitempty" minimum:"1" maximum:"1000" doc:"Default 50"`
	UpdatesPerSecond  *int64 `json:"updates_per_second,omitempty" minimum:"1" maximum:"100" doc:"Default 20"`
}

// NotificationDefaultsInput sets notification_defaults; a field left out takes its default. Link preferences use the
// same shape for a user's own overrides (LinkPrefs).
type NotificationDefaultsInput struct {
	NewActions       *bool   `json:"new_actions,omitempty" doc:"Default true"`
	MinPriority      *string `json:"min_priority,omitempty" enum:"low,normal,high,urgent" doc:"Default normal"`
	AssignedToMe     *bool   `json:"assigned_to_me,omitempty" doc:"Default true"`
	Escalations      *bool   `json:"escalations,omitempty" doc:"Default true"`
	Reminders        *string `json:"reminders,omitempty" enum:"push,digest,off" doc:"Default push"`
	Clarifications   *bool   `json:"clarifications,omitempty" doc:"Default true"`
	OperatorMessages *bool   `json:"operator_messages,omitempty" doc:"Default true"`
	InboxItems       *string `json:"inbox_items,omitempty" enum:"push,digest,off" doc:"Default digest"`
	TasksInReview    *string `json:"tasks_in_review,omitempty" enum:"push,digest,off" doc:"Default digest"`
	FailedRuns       *string `json:"failed_runs,omitempty" enum:"push,digest,off" doc:"Default digest"`
	Digest           *bool   `json:"digest,omitempty" doc:"Default true"`
	DigestTime       *string `json:"digest_time,omitempty" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM, default 09:00"`
	QuietHours       *bool   `json:"quiet_hours,omitempty" doc:"Default true"`
	QuietHoursStart  *string `json:"quiet_hours_start,omitempty" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM, default 23:00"`
	QuietHoursEnd    *string `json:"quiet_hours_end,omitempty" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM, default 08:00"`
}

func set[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// Resolve returns the defaults with every field in in set.
func Resolve(in Input) Settings {
	s := Defaults()
	s.Apply(in)
	return s
}

// Apply sets every field in in. Settings' methods have pointer receivers: the API embeds Settings in SettingsOut, and
// huma's schema links can't wrap an embedded type with value methods.
func (s *Settings) Apply(in Input) {
	set(&s.LinkDefaultMaxRole, in.LinkDefaultMaxRole)
	set(&s.LinkAllowAdminCap, in.LinkAllowAdminCap)
	set(&s.LinkSSOGrants, in.LinkSSOGrants)
	set(&s.LinkReconfirmDays, in.LinkReconfirmDays)
	set(&s.LinksPerUser, in.LinksPerUser)
	set(&s.LinkCodeTTLSeconds, in.LinkCodeTTLSeconds)
	set(&s.InitDataMaxAgeSeconds, in.InitDataMaxAgeSeconds)
	set(&s.MiniAppSessionIdleSeconds, in.MiniAppSessionIdleSeconds)
	set(&s.MiniAppSessionAbsoluteSeconds, in.MiniAppSessionAbsoluteSeconds)
	set(&s.TopicsModeDefault, in.TopicsModeDefault)
	if l := in.SendLimits; l != nil {
		set(&s.SendLimits.GlobalPerSecond, l.GlobalPerSecond)
		set(&s.SendLimits.ChatPerSecond, l.ChatPerSecond)
		set(&s.SendLimits.ChatBurst, l.ChatBurst)
		set(&s.SendLimits.GroupPerMinute, l.GroupPerMinute)
		set(&s.SendLimits.QueueTotal, l.QueueTotal)
		set(&s.SendLimits.QueuePerChat, l.QueuePerChat)
	}
	if l := in.InboundLimits; l != nil {
		set(&s.InboundLimits.ActionsPerMinute, l.ActionsPerMinute)
		set(&s.InboundLimits.ActionsBurst, l.ActionsBurst)
		set(&s.InboundLimits.InboxItemsPerHour, l.InboxItemsPerHour)
		set(&s.InboundLimits.DownloadsPerHour, l.DownloadsPerHour)
		set(&s.InboundLimits.UpdatesPerSecond, l.UpdatesPerSecond)
	}
	if n := in.NotificationDefaults; n != nil {
		s.NotificationDefaults = s.NotificationDefaults.Apply(*n)
	}
}

// Apply returns d with every field in in set.
func (d NotificationDefaults) Apply(in NotificationDefaultsInput) NotificationDefaults {
	set(&d.NewActions, in.NewActions)
	set(&d.MinPriority, in.MinPriority)
	set(&d.AssignedToMe, in.AssignedToMe)
	set(&d.Escalations, in.Escalations)
	set(&d.Reminders, in.Reminders)
	set(&d.Clarifications, in.Clarifications)
	set(&d.OperatorMessages, in.OperatorMessages)
	set(&d.InboxItems, in.InboxItems)
	set(&d.TasksInReview, in.TasksInReview)
	set(&d.FailedRuns, in.FailedRuns)
	set(&d.Digest, in.Digest)
	set(&d.DigestTime, in.DigestTime)
	set(&d.QuietHours, in.QuietHours)
	set(&d.QuietHoursStart, in.QuietHoursStart)
	set(&d.QuietHoursEnd, in.QuietHoursEnd)
	return d
}

var priorities = map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}

func delivery(v string) bool { return v == DeliveryPush || v == DeliveryDigest || v == DeliveryOff }

var reClock = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// ParseClock parses a wall-clock time "HH:MM" into minutes after midnight.
func ParseClock(s string) (int, error) {
	if !reClock.MatchString(s) {
		return 0, fmt.Errorf("must be a time of day as HH:MM, got %q", s)
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h*60 + m, nil
}

// Validate reports every field outside its bounds, with its JSON path.
func (s *Settings) Validate() error {
	v := &apperr.Validation{}
	rng := func(field string, x, lo, hi int64) {
		if x < lo || x > hi {
			v.Add(field, "must be between %d and %d, got %d", lo, hi, x)
		}
	}
	if s.LinkDefaultMaxRole != RoleViewer && s.LinkDefaultMaxRole != RoleOperator {
		v.Add("link_default_max_role", "must be viewer or operator")
	}
	rng("link_reconfirm_days", s.LinkReconfirmDays, MinReconfirmDays, MaxReconfirmDays)
	rng("links_per_user", s.LinksPerUser, MinLinksPerUser, MaxLinksPerUser)
	rng("link_code_ttl_seconds", s.LinkCodeTTLSeconds, MinLinkCodeTTL, MaxLinkCodeTTL)
	rng("init_data_max_age_seconds", s.InitDataMaxAgeSeconds, MinInitDataMaxAge, MaxInitDataMaxAge)
	rng("miniapp_session_idle_seconds", s.MiniAppSessionIdleSeconds, MinMiniAppIdle, MaxMiniAppIdle)
	rng("miniapp_session_absolute_seconds", s.MiniAppSessionAbsoluteSeconds, MinMiniAppAbsolute, MaxMiniAppAbsolute)
	if s.MiniAppSessionAbsoluteSeconds < s.MiniAppSessionIdleSeconds {
		v.Add("miniapp_session_absolute_seconds", "must be at least miniapp_session_idle_seconds (%d)", s.MiniAppSessionIdleSeconds)
	}
	sl := s.SendLimits
	rate := func(field string, x, hi float64) {
		if math.IsNaN(x) || x <= 0 || x > hi {
			v.Add(field, "must be above 0 and at most %g, got %g", hi, x)
		}
	}
	rate("send_limits.global_per_second", sl.GlobalPerSecond, sender.MaxGlobalPerSecond)
	rate("send_limits.chat_per_second", sl.ChatPerSecond, sender.MaxChatPerSecond)
	rng("send_limits.chat_burst", sl.ChatBurst, 1, sender.MaxChatBurst)
	rng("send_limits.group_per_minute", sl.GroupPerMinute, 1, sender.MaxGroupPerMinute)
	rng("send_limits.queue_total", sl.QueueTotal, 1, sender.MaxQueueTotal)
	rng("send_limits.queue_per_chat", sl.QueuePerChat, 1, sender.MaxQueuePerChat)
	if sl.QueuePerChat > sl.QueueTotal {
		v.Add("send_limits.queue_per_chat", "must not exceed send_limits.queue_total (%d)", sl.QueueTotal)
	}
	il := s.InboundLimits
	rng("inbound_limits.actions_per_minute", il.ActionsPerMinute, 1, MaxActionsPerMinute)
	rng("inbound_limits.actions_burst", il.ActionsBurst, 1, MaxActionsBurst)
	rng("inbound_limits.inbox_items_per_hour", il.InboxItemsPerHour, 1, MaxInboxItemsPerHour)
	rng("inbound_limits.downloads_per_hour", il.DownloadsPerHour, 1, MaxDownloadsPerHour)
	rng("inbound_limits.updates_per_second", il.UpdatesPerSecond, 1, MaxUpdatesPerSecond)
	s.NotificationDefaults.validate(v, "notification_defaults.")
	return v.Err()
}

func (d NotificationDefaults) validate(v *apperr.Validation, prefix string) {
	if !priorities[d.MinPriority] {
		v.Add(prefix+"min_priority", "must be low, normal, high or urgent")
	}
	for f, x := range map[string]string{"reminders": d.Reminders, "inbox_items": d.InboxItems, "tasks_in_review": d.TasksInReview, "failed_runs": d.FailedRuns} {
		if !delivery(x) {
			v.Add(prefix+f, "must be push, digest or off")
		}
	}
	for f, x := range map[string]string{"digest_time": d.DigestTime, "quiet_hours_start": d.QuietHoursStart, "quiet_hours_end": d.QuietHoursEnd} {
		if _, err := ParseClock(x); err != nil {
			v.Add(prefix+f, "%v", err)
		}
	}
	if d.QuietHoursStart == d.QuietHoursEnd {
		v.Add(prefix+"quiet_hours_end", "must differ from quiet_hours_start")
	}
}

// Sender returns the outbound sender's limits.
func (l SendLimits) Sender() sender.Limits {
	return sender.Limits{GlobalPerSecond: l.GlobalPerSecond, ChatPerSecond: l.ChatPerSecond, ChatBurst: int(l.ChatBurst),
		GroupPerMinute: int(l.GroupPerMinute), QueueTotal: int(l.QueueTotal), QueuePerChat: int(l.QueuePerChat)}
}
