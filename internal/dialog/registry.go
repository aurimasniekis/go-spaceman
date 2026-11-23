package dialog

import "regexp"

// CodeModeEnv is the settings env variable that turns Claude Code's experimental Code Mode on: a session started with
// it asks the "Code Mode (experimental)" opt-in question (docs/contracts/harnesses.md).
const CodeModeEnv = "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE"

// themeDefault is the theme picker's default option (2.1.328), the one SpaceMan selects.
const themeDefault = "Auto (match terminal)"

// Registry is the known dialogs SpaceMan answers (ADR-0102). An entry is added only for a dialog whose screen text was
// seen or read from the harness, with an answer that grants nothing SpaceMan did not already provision. The never-answer
// list (Never) is checked before any entry, so an entry can never answer a permission, tool-approval or login dialog.
var Registry = []Entry{
	{
		// Claude Code's first-run theme picker (a fresh config directory). SpaceMan seeds a theme (ADR-0021), so this is
		// the safety net: it selects the default text style, "Auto (match terminal)". Only the picker's own layout
		// matches: a block of options without footer or numbers (Weak) that offers the dark and light styles. A numbered
		// or footed dialog that quotes the sentence is not the picker.
		ID: "claude.theme_picker", Harness: "claude-code", MinVersion: "2.1.316",
		Text: []string{"Choose the text style that looks best with your terminal"},
		Answer: func(d Dialog, _ Context) (Answer, bool) {
			if !d.Weak || !d.hasOptions(themeDefault, "Dark mode", "Light mode") {
				return Answer{}, false
			}
			keys, ok := d.Select(themeDefault)
			return Answer{Choice: "default", Keys: keys}, ok
		},
	},
	{
		// "Adding a new directory" for a directory passed with --add-dir that is not trusted (from 2.1.326). Accepted
		// only for a directory SpaceMan provisioned for the run (its memory folder): any other path is flagged.
		ID: "claude.add_dir_trust", Harness: "claude-code", MinVersion: "2.1.326",
		Text:    []string{"Adding a new directory:", "This session hasn't worked here before"},
		Options: []string{"No, exit", "Yes, add it"},
		Answer: func(d Dialog, c Context) (Answer, bool) {
			path, ok := d.Between("Adding a new directory:", "This session hasn't worked here before")
			if !ok || !c.provisioned(path) {
				return Answer{}, false
			}
			keys, ok := d.Select("Yes, add it")
			return Answer{Choice: "yes", Keys: keys}, ok
		},
	},
	{
		// The Code Mode opt-in (CLAUDE_CODE_EXPERIMENTAL_CODE_MODE set). Turned on only when the run's own profile set the
		// variable; otherwise the session starts without it.
		ID: "claude.code_mode_optin", Harness: "claude-code", MinVersion: "2.1.325",
		Text:    []string{"Code Mode (experimental)", "start without it"},
		Options: []string{"No, start without it", "Yes, turn on Code Mode"},
		Answer: func(d Dialog, c Context) (Answer, bool) {
			if c.CodeMode {
				keys, ok := d.Select("Yes, turn on Code Mode")
				return Answer{Choice: "yes", Keys: keys}, ok
			}
			keys, ok := d.Select("No, start without it")
			return Answer{Choice: "no", Keys: keys}, ok
		},
	},
	{
		// "Managed settings require approval" for an organisation's server-managed settings that only redirect
		// telemetry (ADR-0108, managed.go). Accepted only when every destination it names is one the node's operator
		// trusts (harness.claude.trusted_telemetry_endpoints) and the dialog holds nothing but telemetry destinations;
		// otherwise it is flagged. Two of its lines are on the never-answer list ("approval", "Approving"): its title and
		// the telemetry-only summary line are exempt in full, and any other listed word on the dialog still flags it.
		ID: "claude.managed_settings_telemetry", Harness: "claude-code", MinVersion: "2.1.323",
		Text:        []string{managedTitle, managedOnly},
		Options:     []string{managedYes, managedNo},
		NeverExempt: []*regexp.Regexp{regexp.MustCompile(regexp.QuoteMeta(managedTitle)), reApplies},
		Answer: func(d Dialog, c Context) (Answer, bool) {
			if !c.trustsTelemetry(d) {
				return Answer{}, false
			}
			keys, ok := d.Select(managedYes)
			return Answer{Choice: "yes", Keys: keys}, ok
		},
	},
}
