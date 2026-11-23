# Dialog screen fixtures

Screens as `terminal.read` returns them (the visible screen, no history), one file per dialog.

| File | Source |
|---|---|
| `claude-2.1.328-theme.txt` | Captured from Claude Code 2.1.328 in an isolated herdr-core (fresh config directory, no credentials). |
| `claude-2.1.328-login.txt` | Captured the same way after the theme picker was answered: the login method choice (never answered). |
| `claude-2.1.328-add-dir.txt` | Captured the same way with `--add-dir` naming an untrusted directory; the path was replaced with a generic one. |
| `claude-2.1.328-main-idle.txt` | Captured the same way after the add-dir dialog was answered: the main prompt, not a dialog. |
| `claude-2.1.328-code-mode.txt` | Reconstructed from the screen text recorded on a live run ("Code Mode (experimental) … ❯ No, start without it / Yes, turn on Code Mode … Enter to confirm · Esc to start without it"); the body line is a placeholder. |
| `claude-permission.txt` | Constructed in the layout of a Claude Code tool-permission dialog: it must never be answered. |
| `claude-unknown.txt` | Constructed: a dialog the registry does not know. |
| `claude-2.1.323-managed-settings-telemetry.txt` | Captured from a live Claude Code 2.1.323 run (`claude setup-token` login of an organisation with server-managed telemetry settings), from the dialog's top rule down; the collector URL was replaced with `https://telemetry.example.com/prod`. |
