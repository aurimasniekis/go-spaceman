package proto

import "time"

// MinorUsageLimit is the first minor version that carries usage limits (ADR-0079): a run report may carry the limit the
// node detected in the run's transcript (RunReport.Limit). A node below it never reports a limit, and central ignores a
// limit on a report from a session negotiated below it (RunReport.ForMinor).
const MinorUsageLimit = 12

// Usage-limit kinds (RunLimit.Kind): Claude Code's rateLimitType values, the two gateway cases and unknown (the
// wrap-up path carries no kind). Anything else is reported as unknown.
const (
	LimitFiveHour       = "five_hour"
	LimitSevenDay       = "seven_day"
	LimitSevenDayOpus   = "seven_day_opus"
	LimitSevenDaySonnet = "seven_day_sonnet"
	// LimitSevenDayOverage is Claude Code's seven_day_overage_included (in the binary, not yet seen in a transcript).
	LimitSevenDayOverage = "seven_day_overage_included"
	LimitOverage         = "overage"
	LimitGatewayQuota    = "gateway_quota"
	LimitGatewayBudget   = "gateway_budget"
	LimitUnknown         = "unknown"
)

// LimitKinds is the allowlist of usage-limit kinds, the same as the CHECK on usage_limits.kind (runs.limit_kind has
// none, so a kind added later needs no runs-table rebuild).
var LimitKinds = []string{LimitFiveHour, LimitSevenDay, LimitSevenDayOpus, LimitSevenDaySonnet, LimitSevenDayOverage, LimitOverage,
	LimitGatewayQuota, LimitGatewayBudget, LimitUnknown}

// MaxLimitReset bounds how far a reported reset may be from when the limit was seen, and how long ago that may be: a
// reset outside it is reported as unknown, so a forged transcript record holds an account for at most this long
// (ADR-0079).
const MaxLimitReset = 8 * 24 * time.Hour

// LimitKind maps a reported kind onto the allowlist: an unlisted value becomes LimitUnknown.
func LimitKind(k string) string {
	for _, v := range LimitKinds {
		if k == v {
			return k
		}
	}
	return LimitUnknown
}

// RunLimit is a run's usage limit as the node detected it from typed transcript fields (minor ≥ 12). It never carries
// transcript text. Cleared reports that the limit is over (Kind and ResetsAt are then empty): omitempty on
// RunReport.Limit can't express clearing.
type RunLimit struct {
	Kind string `json:"kind,omitempty"`
	// ResetsAt is when the limit resets, in (Since, Since+MaxLimitReset], possibly already past; nil when unknown.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Since is when the node first saw the limit, within [now-MaxLimitReset, now].
	Since   time.Time `json:"since,omitzero"`
	Cleared bool      `json:"cleared,omitempty"`
}

// Normalized returns l as it may be stored or sent, and false when it is not a limit at all (not cleared and no kind).
// Since is bounded to [now-MaxLimitReset, now] (zero: now). The kind is mapped onto the allowlist. A reset outside
// (Since, Since+MaxLimitReset] becomes unknown; one that already passed but is inside it is kept, so a limit seen
// before a reset is not turned into an open unknown one. A cleared limit carries no kind and no reset.
func (l RunLimit) Normalized(now time.Time) (RunLimit, bool) {
	if !l.Cleared && l.Kind == "" {
		return RunLimit{}, false
	}
	since := l.Since
	switch earliest := now.Add(-MaxLimitReset); {
	case since.IsZero(), since.After(now):
		since = now
	case since.Before(earliest):
		since = earliest
	}
	since = since.UTC()
	if l.Cleared {
		return RunLimit{Since: since, Cleared: true}, true
	}
	out := RunLimit{Kind: LimitKind(l.Kind), Since: since}
	if r := l.ResetsAt; r != nil && r.After(since) && !r.After(since.Add(MaxLimitReset)) {
		v := r.UTC()
		out.ResetsAt = &v
	}
	return out, true
}

// ForMinor returns the report as a session negotiated at minor may carry it: below MinorUsageLimit it has no limit.
// The node applies it before sending, central on receipt.
func (r RunReport) ForMinor(minor int) RunReport {
	if minor < MinorUsageLimit {
		r.Limit = nil
	}
	return r
}
