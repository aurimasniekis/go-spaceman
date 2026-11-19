package dialog

// CodeModeEnv is the settings env variable that turns Claude Code's experimental Code Mode on: a session started with
// it asks the "Code Mode (experimental)" opt-in question (docs/contracts/harnesses.md).
const CodeModeEnv = "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE"

// Registry is the known dialogs SpaceMan answers (ADR-0102). An entry is added only for a dialog whose screen text was
// seen or read from the harness, with an answer that grants nothing SpaceMan did not already provision. The never-answer
// list (Never) is checked before any entry, so an entry can never answer a permission, tool-approval or login dialog.
var Registry = []Entry{
	{
		// Claude Code's first-run theme picker (a fresh config directory). SpaceMan seeds a theme (ADR-0021), so this is
		// the safety net: Enter keeps the default text style.
		ID: "claude.theme_picker", Harness: "claude-code", MinVersion: "2.1.316",
		Text: []string{"Choose the text style that looks best with your terminal"},
		Answer: func(d Dialog, _ Context) (Answer, bool) {
			return Answer{Choice: "default", Keys: []string{"enter"}}, true
		},
	},
	{
		// "Adding a new directory" for a directory passed with --add-dir that is not trusted (from 2.1.326). Accepted
		// only for a directory SpaceMan provisioned for the run (its memory folder): any other path is flagged.
		ID: "claude.add_dir_trust", Harness: "claude-code", MinVersion: "2.1.326",
		Text: []string{"Adding a new directory:", "This session hasn't worked here before"},
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
		Text: []string{"Code Mode (experimental)", "start without it"},
		Answer: func(d Dialog, c Context) (Answer, bool) {
			if c.CodeMode {
				keys, ok := d.Select("Yes, turn on Code Mode")
				return Answer{Choice: "yes", Keys: keys}, ok
			}
			keys, ok := d.Select("No, start without it")
			return Answer{Choice: "no", Keys: keys}, ok
		},
	},
}
