package harness

import (
	"encoding/json"
	"io"
	"regexp"
)

// ClaudePermissionDenyMessage is what SpaceMan's PermissionRequest hook tells the model when it denies a permission
// dialog (ADR-0068).
const ClaudePermissionDenyMessage = "This SpaceMan run is unattended: nobody can answer a permission dialog in its terminal, so SpaceMan " +
	"denied this request. Do not retry it or work around it. If you need it, ask a person with spaceman_human_ask " +
	"(an approval or a question), or tell your control with spaceman_message_send, and carry on with other work " +
	"until you are notified of the answer."

// maxHookInput bounds how much of the hook's input is read.
const maxHookInput = 1 << 20

// reToolName is what a tool name may look like to be repeated in the message (Claude Code tools and mcp__server__tool).
var reToolName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// ClaudePermissionRequestHook is the body of `spaceman-agent hook permission-request`, the command hook every
// SpaceMan-launched Claude Code run has for the PermissionRequest event (ADR-0068). It reads the event's input from in
// and writes a decision to out that denies the request, with a message that points the model at spaceman_human_ask
// instead of retrying. It never allows anything: whatever the input (even none, or input that is not JSON), the answer
// is a deny, so a dialog never waits for a person who is not there.
//
// Output (verified against Claude Code 2.1.317's hook output schema, docs/contracts/harnesses.md):
//
//	{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"…"}}}
//
// interrupt is left unset (false), so the model gets the message and the turn continues.
func ClaudePermissionRequestHook(in io.Reader, out io.Writer) error {
	var input struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
	}
	if in != nil {
		b, _ := io.ReadAll(io.LimitReader(in, maxHookInput))
		_ = json.Unmarshal(b, &input)
	}
	msg := ClaudePermissionDenyMessage
	if reToolName.MatchString(input.ToolName) {
		msg = "Permission for " + input.ToolName + " denied. " + msg
	}
	return json.NewEncoder(out).Encode(permissionDeny(msg))
}

func permissionDeny(msg string) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PermissionRequest",
		"decision":      map[string]any{"behavior": "deny", "message": msg},
	}}
}

// ClaudeStaticPermissionDeny is the decision the hook command prints when the agent binary fails (see
// ClaudePermissionHookCommand): the same deny, without the tool name.
func ClaudeStaticPermissionDeny() string {
	b, _ := json.Marshal(permissionDeny(ClaudePermissionDenyMessage))
	return string(b)
}
