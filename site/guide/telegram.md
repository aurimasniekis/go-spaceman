# Telegram

SpaceMan can reach you in Telegram: a **bot** you chat with, and a **Mini App** that opens inside Telegram for the
pages that need a screen (ADR-0083). Both act **as you**, with the permissions of your own account and never wider. What
Telegram can't do at all is listed in [Limitations](/reference/limitations#telegram-mini-app); the security model is in
[Security](/security/index) and the [threat model](/security/threat-model).

::: warning Status
Everything here is built and tested against a fake Telegram server. **Nothing has been verified against a real bot yet**
(see [Project status](/guide/status)).
:::

An administrator has to set the bot up first ([Telegram setup](/operators/telegram)), and each space is **off for
Telegram until someone with `space_config:write` (operators and up) turns it on**. A space that isn't enabled, or whose content level is `off`, is
invisible to Telegram: it isn't listed, it sends no notifications and the bot answers as if it did not exist.

## Link your Telegram account

Linking happens on **Account → Telegram** in the SpaceMan web app, signed in as yourself.

1. Choose **Link Telegram**. You are asked to sign in again if your session is not fresh. SpaceMan shows a one-time code
   (valid for 10 minutes by default).
2. In Telegram, open the bot and start it with the code (`/start <code>`). The bot names the SpaceMan address, your name and a masked e-mail, and asks you to confirm
   (or tap "This wasn't me").
3. Back in the web page, which updates by itself, SpaceMan shows the Telegram account that is asking (its name,
   username and ID). Check that it is yours and **Confirm** it (this needs a recent sign-in).

Both sides confirm on purpose: a leaked code can't bind someone else's account, and a code an attacker sends you can't
bind your account to theirs without you seeing whose it is. Only the browser session that created the code can finish
it, and API tokens can't link.

What a link can do:

- It acts as **you**, capped. By default a link is capped at the **operator** role in each space you can read, and never
  above your own current grants: if you lose a grant, the link loses it too, at once. An administrator may set the default cap
  to viewer, or allow "same as my grants".
- You can have one Telegram account linked by default (an administrator may allow up to three). One Telegram account
  links to one SpaceMan user.
- A link must be confirmed again from the web every 90 days by default. **Account → Telegram** shows when, and lets you
  reconfirm or remove the link.
- A user who still has to set up two-factor authentication gets a **limited** link, like their web session: it can't be
  used to get around the setup.
- `/unlink` in the chat, **Account → Telegram**, or an administrator ends a link at once. Disabling the user, or
  configuring a different bot, does too.

Telegram never signs you in to SpaceMan and can't change your password, tokens or two-factor settings.

## The bot chat

Messages to the bot are private chats only (groups: [Group mode](#group-mode)). Commands appear in Telegram's command
menu. Most act on a **current space**: the one named in the command (`/queue payments`), else the one you picked with
`/space`, else your only visible space.

| Command | What it does |
|---|---|
| `/spaces`, `/space [name]` | List the spaces Telegram may show you with their counts, or switch the current space. |
| `/needs` (also `/needsyou`) | What waits for you: [questions, to-dos, messages, approvals and custom forms](#answer-needs-you-forms-and-approvals). |
| `/inbox` | The current space's inbox with status filters. Open an item to read it, with its first attachments. |
| `/queue` | What runs, what is ready and what waits. |
| `/runs` | Live runs, and **Stop** a run. |
| `/messages` | What agents sent to the operators. |
| `/control` | Talk to the space's control run. |
| `/settings` | Opens your [Telegram settings](#the-mini-app) in the Mini App (a button; without a Mini App host the bot says it isn't set up). |
| `/help` | Lists these commands. |
| `/unlink` | End this link. |

The command menu the bot registers and the commands it answers come from one table, so the menu can't list a command
nothing handles. `/needsyou` is the long name of `/needs` and stays out of the menu. There is no `/new` or `/digest`:
creating a task is in the Mini App, and the digest arrives on its own ([Notifications](#notifications-quiet-hours-and-digest)).

### Capture to the inbox

Any message that isn't a command becomes an **inbox item** in the current space, as you (the source is shown as
"Telegram" next to your name):

- Text, or a photo or file's caption, is the body. Photos and files become attachments (up to 20 MB, checked like any
  upload). A forwarded message adds a "Forwarded from" line, and an album becomes one item.
- The bot acknowledges every capture. If you see no acknowledgement, send it again: an update that was in flight when
  central restarted can be lost.

### Queue, runs and messages

- **Queue.** Change a task's status, place it (top, up, down, bottom) and decide an incident (promote or demote) after a
  confirmation. The card's version travels with the action as `If-Match` (also on acknowledge and resolve of a message), so
  if someone else changed the task or message first the API answers 409 `precondition_failed`, nothing changes, and you get
  a fresh card instead of a stale action. There is no task creation or run launch in chat: **New task** is in the Mini App,
  for operators.
- **Runs.** Live runs; **Stop** needs a confirmation and the `run:control` permission.
- **Messages.** Reply to, acknowledge or resolve what agents sent to operators.
- **Control.** `/control` starts control mode until `/control off`: your plain text becomes a draft with **Send to control** and
  **Cancel**, and the control run's replies come back to the chat. Files, images and forwards still go to the inbox.

Terminal input, takeover, run launches, secrets, accounts, users and every administration page stay in the web
([Limitations](/reference/limitations#telegram-mini-app)).

## Answer needs-you forms and approvals

`/needsyou` lists pending [human actions](/guide/human-actions) across your spaces, and the bot sends a card for each new
one that you are allowed to see (see [Notifications](#notifications-quiet-hours-and-digest)). You answer **as you**, with
the same checks as the web:

- **Questions** (options and free text), **to-dos** (steps), **messages** and **custom types** with a simple response
  schema (a step-by-step form).
- Common actions on any card: claim or release it, comment, **ask the requester** a clarification (which never answers
  the request), and **withdraw** it after a confirmation tied to that card's version. A card that went stale is redrawn,
  never overwritten.
- An **advisory** approval: **Approve → Confirm**, and a denial asks for a note.
- An **enforced** approval ([Enforced approvals](/guide/approvals)) shows what is being approved (operation, resource,
  scope, expiry and uses, the argument digest, never the arguments) and, by default, only a **Decide in browser**
  button. Someone with `space_config:write` (operators and up) may opt a space in with `enforced_approvals = confirm`. Then deciding in chat takes a
  second step, and the **server** checks that the first step happened, so a replayed or forged tap decides nothing. The
  authorization is checked again when you confirm. Bulk approval and withdrawing an approval stay out of Telegram.
- A custom form too complex for chat shows **Open in the app** and continues in the Mini App.

## The Mini App

The bot's menu button opens the **Mini App**, served from its own host so that it never shares cookies with the web app.
It signs you in with the data Telegram gives it, only for a linked account. Its session lives in memory: after (by default) 15
minutes idle, an hour in all, or a restart, it shows **Session ended — close and reopen the app**.

Pages:

- **Overview**: needs-attention count, active runs and spaces.
- **Space overview** and **Needs you**: the forms for every human-action type, with inbox cards and **View diagram** for
  Mermaid.
- **Inbox**, with images shown inline and attachments as one-time download links.
- **Queue**: the same list as the bot, drag to reorder, the task sheet, and **New task** (operators). A read-only
  **Next** card shows what the queue picks next, without recording the decision.
- **Runs** and a run's page. With the space's `terminal_watch` on, the run page shows the run's **live terminal,
  watch-only** ([below](#spaces-content-levels-and-the-terminal)); otherwise the page says the terminal isn't available there.
- **Telegram settings** (`/settings` opens it): this link's access and session, and your notification settings:
  categories, quiet hours, digest, muted spaces and your time zone (which is per user). A save sends the form's version, so a
  save from a stale form reloads it with a conflict notice. A space's topics setting is shown, not edited there.

Content at the `summary` level goes to the Mini App only (phone to central, never through Telegram).

<!-- Screenshots: the capture pipeline (make screenshots, docs/SCREENSHOTS.md) runs the web app only and has no Mini App
     mode, so the Mini App pages have no screenshots yet. Add them with the pipeline's extension, not by hand. -->

*Screenshots of the Mini App are not included yet: the site's screenshot pipeline captures the web app only.*

## Spaces, content levels and the terminal

Someone with `space_config:write` (operators and up) controls what Telegram may see, per space, with [`spaceman telegram set-space`](/reference/cli/telegram#spaceman-telegram-set-space), the API (there is no
web page for these settings):

| Setting | Values | Meaning |
|---|---|---|
| `enabled` | off (default), on | Whether the space is visible through Telegram at all. |
| `content_level` | `full` (default once enabled), `summary`, `off` | `full` shows bodies in chat; `summary` keeps bodies in the Mini App; `off` hides the space. |
| `enforced_approvals` | `web_only` (default), `confirm` | Whether an enforced approval may be decided in chat. |
| `topics` | off (default) | A topic per space ([Topics mode](#topics-mode)). |
| `terminal_watch` | off (default) | Shows the run's live terminal, read-only, in the Mini App ([below](#the-watch-only-terminal)). |
| `group_chat_id` | none (default) | The forum group bound to the space ([Group mode](#group-mode)); changing it needs the admin-only `telegram_group:write`. |

### The watch-only terminal

When `terminal_watch` is on for a space, a run's page in the Mini App shows its live terminal to a link that holds
`terminal:watch` in that space (operators and up, within the link's cap). It is **read-only**: no keys, paste, resizing or
takeover, and no replay of earlier output. The stream comes straight from central over the Mini App host and never goes
through Telegram. Central checks the gate again on every stream, not just the page: a space that hasn't opted in, or a link
without the permission, gets nothing. Turning `terminal_watch` off, disabling or hiding the space for Telegram, or archiving or
deleting it **ends the terminal streams already open at once**; they don't wait for their next periodic check.

Never sent to a chat at any level: terminal output (the Mini App's terminal is the only place it goes, as a stream from central), responses marked hidden, the contents of attachments that aren't
images or documents SpaceMan allows (a link only), and secrets. Messages with a body are sent with forwarding and saving
turned off (a deterrent, not protection from screenshots). Chat messages are stored on Telegram's servers and aren't
end-to-end encrypted: choose `summary` or `off` for spaces whose content must not go there.

## Topics mode

In a private chat with topics enabled for the bot (in BotFather), a space with `topics` on gets its **own topic**, and what
needs you goes to a **Needs you** topic. The bot creates a topic when it first has something to put there, and remembers it
for your link. A message you write inside a space's topic works on that space without changing your current space.
`/space` still switches it explicitly. If a topic was deleted or closed, the bot makes it again once, and otherwise falls
back to the main chat, so a notification isn't lost. If Telegram refuses topics in your chat, the bot uses the main chat and
asks again after 10 minutes. Topics only choose where a message appears, never whether you may see it.

The per-space `topics` setting is the switch, turned on by someone with `space_config:write`. The Mini App's settings page
shows it read-only: which of your spaces use topics, and a note to enable topics for the bot in BotFather when Telegram refused
a topic in your chat.

The global `topics_mode_default` is stored with the settings, but nothing reads
it yet, so it changes nothing.

## Group mode

A **forum group** (a supergroup with topics turned on) can be bound to **one space**. In a bound group the bot posts that
space's needs-you cards and answers a few commands, always at the **summary** level.

**Binding.** Add the bot to the group, then, as someone with `space_config:write` (operators and up), bind it with the
space's `group_chat_id` (`spaceman telegram set-space --group-chat-id <negative chat ID>`; `0` unbinds). Binding, rebinding
and changing `group_chat_id` on a write (including `0`) need the admin-only `telegram_group:write` permission on top of
`space_config:write` and a step-up; an operator who may write the space's other Telegram settings can't change the group.
Central asks Telegram that the bot is a member and that the chat is a supergroup with topics (otherwise 422), and one group
serves one space (409 if it is bound elsewhere). **Removing the space's Telegram settings (`space_config:write`, which
operators hold) also unbinds the group.** Writing the other settings leaves the binding alone.

In a bound group:

- **Every tap is yours.** The bot checks the person who tapped against **their own link**, not the person who bound the
  group and not the group. A member without a link, or whose account can't read the space, is refused, and a viewer can't
  do an operator's action. Group membership grants nothing.
- **Summary only.** A group gets the summary level whatever the space's content level is, and the bot never sends an
  attachment to a group.
- **Taps get private alerts.** A refusal or an answer to a tap is an alert only the person who tapped sees. A command has
  no such reply: `/spaces`, `/space` and `/control` get one short public reply, "This group is bound to one space.", which shows
  nothing of the space. A command from a member whose account can't use the space gets a different reply, "Your SpaceMan
  account can't use this space. Ask an administrator.", which also shows nothing of the space.
- **Silence.** A message that isn't a command of the bot's group set, a command for another bot, plain chatter, and any
  message from a member without a link are ignored without an answer. `/inbox`, `/needsyou`, `/queue`, `/runs` and
  `/messages` are answered, with summaries. `/needs`, `/settings`, `/help` and `/unlink`
  are **not** answered in a group (use the long `/needsyou` there), and capture and control text are never taken.
- **Tied to the tapper.** A form, confirmation or prompt opened by one person can't be completed by another member.
  **Enforced approvals are never decided in a group.**
- **Mini App links are deep links.** Telegram has no `web_app` buttons in groups, so a form that needs the app gets a URL
  button with the bot's direct Mini App link instead (this needs the bot's Main Mini App to be set in BotFather, the bot's
  username and the Mini App host; without them the form has no app button and still works). The link only says where to
  go; the Mini App signs you in as you.
- **Posting limit.** A group is limited to `send_limits.group_per_minute` messages a minute (20 by default).

**When the bot leaves.** The bot won't stay where it can't serve: it leaves at once a chat that can never be bound (a
channel, a basic group, a supergroup without topics), it waits **15 minutes** in an unbound forum group for you to bind it
and then leaves, and it leaves a group **whose binding is lost**: unbound, bound to another group, or its space
disabled, hidden, archived or deleted. While it waits it reads and answers nothing. After a central restart the bot forgets
which groups it was waiting in, so the first message or tap in an unbound group makes it leave at once: bind a group soon
after adding the bot, and add the bot again if it left. Group mode works together with
[topics mode](#topics-mode): a group's space comes from its binding only, and replies stay in the forum topic they answer.

## Notifications, quiet hours and digest

The bot sends **cards** for new needs-you actions (edited as they change, so you don't get repeats), and separate
messages for reminders and escalations. What you get is a mix of per-category choices (push, digest or off), starting from
the administrator's defaults:

| Category | Default |
|---|---|
| New actions (priority normal and above), assigned to me, escalations, clarifications, agent messages to operators | push |
| Reminders | push |
| New inbox items, tasks entering review, failed runs | in the digest |
| Daily digest | on, at 09:00 |
| Quiet hours | on, 23:00–08:00 (sent silently, with no sound) |

Quiet hours and the digest are **wall-clock times in your own time zone**, daylight saving included. **With no time zone
set, both are off**; there is no server-wide fallback zone. Linking saves your browser's time zone if you have none. Set it on the web Account page. You can
mute individual spaces (a per-link setting). The digest is rechecked for access when it is sent, lists titles only, and is kept in memory, so
what was collected is lost if central restarts. A link with nothing collected gets none.

Your notification settings are per link (a link stores only what you changed, so a changed administrator default still
reaches what you didn't touch); the time zone is per user. You set it on the web Account page. The Mini App's **Telegram
settings** page also edits the categories, quiet hours, digest, muted spaces and the time zone; a save made from a stale
form reloads with a conflict notice.


## Administration settings

Administrators set the bot, the policy and the per-space settings through the API or the CLI (Terraform resources are planned; the web app
shows the bot's health on the System page only). They are
all described in [Telegram setup](/operators/telegram#administration-settings): the **bot** (token, mode, Mini App host),
**space** settings (above) and **limits**.
