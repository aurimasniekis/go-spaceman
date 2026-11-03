package cli

import (
	"github.com/spf13/cobra"
)

// GroupID is the cobra group of the remote commands in `spaceman --help`.
const GroupID = "remote"

// Commands returns the remote command groups. Register the global flags on the root with BindFlags.
func (a *App) Commands() []*cobra.Command {
	cmds := []*cobra.Command{
		a.nodeCmd(), a.spaceCmd(), a.runCmd(), a.inboxCmd(), a.taskCmd(), a.humanCmd(), a.scheduleCmd(), a.limitsCmd(),
		a.accountCmd(), a.promptCmd(), a.skillCmd(), a.profileCmd(), a.roleCmd(), a.eventCmd(), a.mcpCmd(), a.approvalPolicyCmd(),
		a.xspaceCmd(), a.userCmd(), a.tokenCmd(), a.auditCmd(), a.systemCmd(), a.whoamiCmd(),
	}
	for _, c := range cmds {
		c.GroupID = GroupID
	}
	return cmds
}

// Operations returns the API operation IDs a command (and its subcommands) call, from their annotations.
func Operations(cmd *cobra.Command) []string {
	var out []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if v := c.Annotations[annOps]; v != "" {
			out = append(out, splitComma(v)...)
		}
		for _, ch := range c.Commands() {
			walk(ch)
		}
	}
	walk(cmd)
	return out
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
