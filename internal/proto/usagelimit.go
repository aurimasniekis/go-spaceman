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
	LimitOverage        = "overage"
	LimitGatewayQuota   = "gateway_quota"
	LimitGatewayBudget  = "gateway_budget"
	LimitUnknown        = "unknown"
)

// LimitKinds is the allowlist of usage-limit kinds, the same as the CHECK on runs.limit_kind and usage_limits.kind.
var LimitKinds = []string{LimitFiveHour, LimitSevenDay, LimitSevenDayOpus, LimitSevenDaySonnet, LimitOverage, LimitGatewayQuota,
	LimitGatewayBudget, LimitUnknown}

// MaxLimitReset bounds how far ahead a reported reset may be: a later or past reset is reported as unknown, so a
// forged transcript record holds an account for at most this long (ADR-0079).
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
	// ResetsAt is when the limit resets, in (Since, Since+MaxLimitReset]; nil when unknown.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Since is when the node first saw the limit.
	Since   time.Time `json:"since,omitzero"`
	Cleared bool      `json:"cleared,omitempty"`
}

// Normalized returns l with its kind on the allowlist and a reset outside (now, now+MaxLimitReset] dropped to
// unknown. A cleared limit carries neither.
func (l RunLimit) Normalized(now time.Time) RunLimit {
	if l.Cleared {
		return RunLimit{Since: l.Since, Cleared: true}
	}
	l.Kind = LimitKind(l.Kind)
	if l.ResetsAt != nil {
		if r := *l.ResetsAt; !r.After(now) || r.After(now.Add(MaxLimitReset)) {
			l.ResetsAt = nil
		} else {
			r = r.UTC()
			l.ResetsAt = &r
		}
	}
	return l
}

// ForMinor returns the report as a session negotiated at minor may carry it: below MinorUsageLimit it has no limit.
// The node applies it before sending, central on receipt.
func (r RunReport) ForMinor(minor int) RunReport {
	if minor < MinorUsageLimit {
		r.Limit = nil
	}
	return r
}
