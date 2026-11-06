package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// GroupID is the cobra group of the remote commands in `spaceman --help`.
const GroupID = "remote"

// Commands returns the remote command groups. Register the global flags on the root with BindFlags.
func (a *App) Commands() []*cobra.Command {
	cmds := []*cobra.Command{
		a.nodeCmd(), a.spaceCmd(), a.runCmd(), a.inboxCmd(), a.taskCmd(), a.humanCmd(), a.scheduleCmd(), a.limitsCmd(),
		a.accountCmd(), a.gitKeyCmd(), a.promptCmd(), a.skillCmd(), a.profileCmd(), a.roleCmd(), a.eventCmd(), a.mcpCmd(), a.approvalPolicyCmd(),
		a.xspaceCmd(), a.userCmd(), a.tokenCmd(), a.auditCmd(), a.systemCmd(), a.whoamiCmd(),
		a.kbCmd(), a.repoCmd(), a.memoryCmd(), a.messageCmd(), a.dreamCmd(), a.approvalCmd(), a.attachmentCmd(),
		a.oidcCmd(), a.identityCmd(), a.sessionCmd(), a.notificationsCmd(), a.mfaCmd(), a.passwordCmd(), a.watchCmd(),
	}
	byName := map[string]*cobra.Command{}
	for _, c := range cmds {
		c.GroupID = GroupID
		byName[c.Name()] = c
	}
	// Commands added to existing groups.
	for path, extra := range map[string][]*cobra.Command{
		"space": a.spaceExtra(), "run": a.runExtra(), "task": a.taskExtra(), "human": a.humanExtra(), "account": a.accountExtra(),
		"prompt": a.promptExtra(), "skill": a.skillExtra(), "user": a.userExtra(), "system": a.systemExtra(),
		"event template": a.eventTemplateExtra(), "event subscription": {a.quarantineCmd()},
		"role": {a.cmd(opts{use: "options", short: "Templates, profiles, prompts and skills a space role can use", op: "list-role-options", scoped: true})},
		"mcp": {a.cmd(opts{use: "space-servers", short: "MCP servers granted in a space, with the roles and tools", op: "list-space-mcp-grants", scoped: true,
			view: view{items: "items", cols: []col{c("SERVER", "server_id"), c("SLUG", "slug"), c("NAME", "name"), c("ROLE", "role"), c("TOOLS", "tools"), c("ENABLED", "enabled")},
				empty: "No MCP servers granted."}})},
	} {
		parts := strings.Fields(path)
		g := byName[parts[0]]
		for _, p := range parts[1:] {
			for _, ch := range g.Commands() {
				if ch.Name() == p {
					g = ch
				}
			}
		}
		g.AddCommand(extra...)
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
