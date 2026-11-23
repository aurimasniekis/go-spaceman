package dialog

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The managed-settings telemetry approval (ADR-0108): Claude Code asks before it applies an organisation's
// server-managed settings that redirect its OpenTelemetry export. Seen on 2.1.323 with a `claude setup-token` login,
// which asks at every session start (the approval is kept for the session only); the screen fixture is
// testdata/claude-2.1.323-managed-settings-telemetry.txt.
const (
	managedTitle      = "Managed settings require approval"
	managedIntro      = "Your organization has configured Claude Code to send telemetry to the destination"
	managedDestHeader = "Telemetry destination"
	managedOnly       = "Only accept if you recognize this as your organization's telemetry collector."
	managedYes        = "Yes, I trust these settings"
	managedNo         = "No, exit Claude Code"
)

var (
	// reTelemetryVar is an OpenTelemetry exporter endpoint variable: the only kind of destination the entry accepts.
	reTelemetryVar = regexp.MustCompile(`^OTEL_EXPORTER_OTLP(_(TRACES|METRICS|LOGS))?_ENDPOINT$`)
	// reApplies is the summary line of a telemetry-only approval: env vars that redirect telemetry, and nothing else.
	reApplies = regexp.MustCompile(`^Approving applies: env vars? ×(\d+) — this redirects where telemetry from this machine is sent\.$`)
)

// telemetryDestinations reads a managed-settings approval that is about telemetry destinations only, and returns the
// destinations it names as NAME=VALUE pairs. It returns false for anything else on the dialog: another section, another
// kind of setting in the summary, a destination that is not an OTLP endpoint variable, or a count that does not match.
// Every line of the dialog must be one of its known parts, so a dialog that also asks about hooks, commands, the
// sandbox or other settings is never read as telemetry-only.
func telemetryDestinations(d Dialog) ([][2]string, bool) {
	const (
		title = iota
		intro
		dests
		applies
		options
	)
	state := title
	var (
		introText, appliesText string
		items                  []string
	)
	for _, raw := range d.Lines {
		l := squash(raw)
		if l == "" {
			if state == applies && appliesText != "" {
				state = options
			}
			continue
		}
		switch state {
		case title:
			if l != managedTitle {
				return nil, false
			}
			state = intro
		case intro:
			if l == managedDestHeader+":" || l == managedDestHeader+"s:" {
				if !strings.HasPrefix(introText, managedIntro) {
					return nil, false
				}
				state = dests
				continue
			}
			introText = strings.TrimSpace(introText + " " + l)
		case dests:
			switch {
			case l == managedOnly:
				state = applies
			case strings.HasPrefix(l, "· "):
				items = append(items, strings.TrimPrefix(l, "· "))
			case len(items) > 0:
				items[len(items)-1] += l // a long destination wrapped onto the next line
			default:
				return nil, false
			}
		case applies:
			appliesText = strings.TrimSpace(appliesText + " " + l)
		case options:
			if !isOptionOrFooter(d, raw) {
				return nil, false
			}
		}
	}
	if state != options || len(items) == 0 {
		return nil, false
	}
	m := reApplies.FindStringSubmatch(appliesText)
	if m == nil {
		return nil, false
	}
	if n, err := strconv.Atoi(m[1]); err != nil || n != len(items) {
		return nil, false
	}
	out := make([][2]string, 0, len(items))
	for _, it := range items {
		name, value, ok := strings.Cut(it, "=")
		if !ok || !reTelemetryVar.MatchString(name) || value == "" || strings.ContainsAny(value, " \t") {
			return nil, false
		}
		out = append(out, [2]string{name, value})
	}
	return out, true
}

// isOptionOrFooter reports whether a dialog line is one of its option lines or its confirm footer.
func isOptionOrFooter(d Dialog, line string) bool {
	if reFooter.MatchString(line) {
		return true
	}
	t := strings.TrimSpace(line)
	if m := reCursor.FindStringSubmatch(line); m != nil {
		t = m[1]
	}
	t = strings.TrimSpace(reNumber.ReplaceAllString(t, ""))
	return slices.Contains(d.Options, t)
}

// trustsTelemetry reports whether every destination of a telemetry-only managed-settings approval is one of the
// operator's trusted endpoints (exact match). No trusted endpoints: nothing is trusted.
func (c Context) trustsTelemetry(d Dialog) bool {
	if len(c.TelemetryEndpoints) == 0 {
		return false
	}
	dests, ok := telemetryDestinations(d)
	if !ok {
		return false
	}
	for _, kv := range dests {
		if !slices.Contains(c.TelemetryEndpoints, kv[1]) {
			return false
		}
	}
	return true
}
