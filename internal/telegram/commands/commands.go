// Package commands is the one table of the bot's commands: the private chat's menu (setMyCommands) and the set of
// commands answered in a group are both derived from it, and chat keeps a handler for each row (tested), so the menu
// can't advertise a command nothing handles. It is a leaf package so that both tgbot and chat can import it.
package commands

import "github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"

// Spec is one command.
type Spec struct {
	Name string
	// Desc is the menu description; "" keeps the command out of the menu (an alias that is still handled).
	Desc string
	// Group: the bot answers it in a group; any other command is silently ignored there.
	Group bool
}

// table lists the commands in menu order. It is unexported so no caller can change it; All returns a copy.
var table = []Spec{
	{"needs", "What needs you", false},
	{"needsyou", "", true}, // the long name of /needs, kept out of the menu
	{"inbox", "Inbox of the current space", true},
	{"queue", "Queue of the current space", true},
	{"runs", "Runs of the current space", true},
	{"messages", "Messages waiting for you", true},
	{"control", "Talk to the control run", true},
	{"spaces", "Your spaces", true},
	{"space", "Switch the current space", true},
	{"settings", "Your Telegram settings", false},
	{"help", "What this bot can do", false},
	{"unlink", "Unlink this Telegram account", false}, // handled by the dispatcher before any link is looked up
}

// All returns a copy of the table, in menu order.
func All() []Spec { return append([]Spec(nil), table...) }

// Menu is the command menu of private chats.
func Menu() []botapi.BotCommand {
	var out []botapi.BotCommand
	for _, c := range table {
		if c.Desc != "" {
			out = append(out, botapi.BotCommand{Command: c.Name, Description: c.Desc})
		}
	}
	return out
}

// Lookup finds a command by its lower-case name.
func Lookup(name string) (Spec, bool) {
	for _, c := range table {
		if c.Name == name {
			return c, true
		}
	}
	return Spec{}, false
}

// IsGroup reports whether the bot answers name in a group.
func IsGroup(name string) bool {
	c, ok := Lookup(name)
	return ok && c.Group
}
