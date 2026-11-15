# Telegram

SpaceMan can reach you in Telegram: a **bot** you chat with, and a **Mini App** that opens inside Telegram for the
pages that need a screen (ADR-0083). Both act **as you**, with the permissions of your own account and never wider. What
Telegram can't do at all is listed in [Limitations](/reference/limitations#telegram-mini-app); the security model is in
[Security](/security/index) and the [threat model](/security/threat-model).

::: warning Status
Everything here is built and tested against a fake Telegram server. **Nothing has been verified against a real bot yet**
(see [Project status](/guide/status)). Sections marked **Not in the base build yet** describe work that is approved and
being merged; they are written from its branch and will change if the review does.
:::

An administrator has to set the bot up first ([Telegram setup](/operators/telegram)), and each space is **off for
Telegram until a space administrator turns it on**. A space that isn't enabled, or whose content level is `off`, is
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
| `/needsyou` | What waits for you: [questions, to-dos, messages, approvals and custom forms](#answer-needs-you-forms-and-approvals). |
| `/inbox` | The current space's inbox with status filters. Open an item to read it, with its first attachments. |
| `/queue` | What runs, what is ready and what waits. |
| `/runs` | Live runs, and **Stop** a run. |
| `/messages` | What agents sent to the operators. |
| `/control` | Talk to the space's control run. |
| `/unlink` | End this link. |

**Menu versus handlers.** The command menu the bot registers also lists `/digest`, `/settings`, `/help` and `/new` (and names the needs-you command
`/needs`). In the base build only the commands in the table above are handled by chat code; the others are not
implemented there. Verify against the bot before relying on them.

### Capture to the inbox

Any message that isn't a command becomes an **inbox item** in the current space, as you (the source is shown as
"Telegram" next to your name):

- Text, or a photo or file's caption, is the body. Photos and files become attachments (up to 20 MB, checked like any
  upload). A forwarded message adds a "Forwarded from" line, and an album becomes one item.
- The bot acknowledges every capture. If you see no acknowledgement, send it again: an update that was in flight when
  central restarted can be lost.

### Queue, runs and messages

- **Queue.** Change a task's status, place it (top, up, down, bottom) and decide an incident (promote or demote) after a
  confirmation. The card's version travels with the action, so if someone else changed the task first you get a conflict
  and a fresh card instead of a stale action. *Version checks on move, acknowledge and resolve depend on the
  [If-Match change](#unlanded-work) (not in the base build yet).* There is no task creation or run launch in chat: **New
  task** is in the Mini App, for operators.
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
  button. A space administrator may opt a space in with `enforced_approvals = confirm`. Then deciding in chat takes a
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
  **Next** card shows what the queue picks next, without recording the decision *(the Next card is part of the
  [settings edit work](#unlanded-work), not in the base build yet)*.
- **Runs** and a run's page, with no terminal yet: the Mini App says the terminal isn't available there. The server-side
  `terminal_watch` gate exists, and a watch-only view (never typing, takeover or sizing) is
  [unlanded work](#unlanded-work).
- **Telegram settings**: this link's access and session; with the [settings edit work](#unlanded-work), also your
  notification settings.

Content at the `summary` level goes to the Mini App only (phone to central, never through Telegram).

<!-- Screenshots: the capture pipeline (make screenshots, docs/SCREENSHOTS.md) runs the web app only and has no Mini App
     mode, so the Mini App pages have no screenshots yet. Add them with the pipeline's extension, not by hand. -->

*Screenshots of the Mini App are not included yet: the site's screenshot pipeline captures the web app only.*

## Spaces, content levels and the terminal

A space administrator controls what Telegram may see, per space, with [`spaceman telegram set-space`](/reference/cli/telegram#spaceman-telegram-set-space), the API (there is no
web page for these settings):

| Setting | Values | Meaning |
|---|---|---|
| `enabled` | off (default), on | Whether the space is visible through Telegram at all. |
| `content_level` | `full` (default once enabled), `summary`, `off` | `full` shows bodies in chat; `summary` keeps bodies in the Mini App; `off` hides the space. |
| `enforced_approvals` | `web_only` (default), `confirm` | Whether an enforced approval may be decided in chat. |
| `topics` | off (default) | A topic per space ([Topics mode](#topics-mode)). |
| `terminal_watch` | off (default) | Lets the server serve a run's terminal stream, read-only, to the Mini App. No Mini App page uses it in the base build ([unlanded work](#unlanded-work)). |

Never sent to a chat at any level: terminal output, responses marked hidden, the contents of attachments that aren't
images or documents SpaceMan allows (a link only), and secrets. Messages with a body are sent with forwarding and saving
turned off (a deterrent, not protection from screenshots). Chat messages are stored on Telegram's servers and aren't
end-to-end encrypted: choose `summary` or `off` for spaces whose content must not go there.

## Topics mode

::: warning Not in the base build yet
Written from the topics branch (#107). It is not merged into the base build.
:::

In a private chat with topics enabled for the bot, a space with `topics` on gets its **own topic**, and what needs you
goes to a **Needs you** topic. The bot creates a topic when it first has something to put there, and remembers it. A
message you write inside a space's topic works on that space without changing your current space. `/space` still
switches it explicitly. If a topic was deleted or closed, the bot makes it again once, and otherwise falls back to the
main chat, so a notification isn't lost. If Telegram refuses topics in your chat, the bot uses the main chat and asks
again after 10 minutes. Topics only choose where a message appears, never whether you may see it.

The branch keys topics mode on the space's `topics` setting. The global `topics_mode_default` ("new links start in topics
mode") exists in the settings but this branch doesn't read it: **control should check how a person opts in or out
before this paragraph is final.**

## Group mode

::: warning Not in the base build yet
Written from the group authorization branch (TG-13-1). Binding a group to a space (the API field `group_chat_id`, the
Terraform attribute and the bind step) comes with TG-13-2 and is **not written here until it exists**. Without it, a
group can't be bound.
:::

Design: a **forum group** can be bound to **one space** by a space administrator (a step-up, `space_config:write`). In a bound
group:

- **Every tap is yours.** The bot checks the person who tapped against **their own link**, not the person who bound the
  group and not the group. A group member without a link is refused, a linked member who can't read the space is told so,
  and a viewer can't do an operator's action. Group membership grants nothing.
- **Summary only.** A group gets the summary level whatever the space's content level is, and the bot never sends an
  attachment to a group.
- **One space.** `/spaces`, `/space`, `/control` and plain-text capture don't apply to a group. `/inbox`, `/needsyou`,
  `/queue`, `/runs` and `/messages` do.
- **Tied to the tapper.** A form, confirmation or prompt opened by one person can't be completed by another member.
  **Enforced approvals are never decided in a group.**
- Telegram has no `web_app` buttons in groups, so there is no one-tap Mini App from a group; open the Mini App from the
  private chat with the bot.

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
reaches what you didn't touch); the time zone is per user. In the base build the web Account page edits the time zone, and
the Mini App operations for the other settings exist without a page for them. With the
[settings edit work](#unlanded-work) the Mini App's **Telegram settings** page edits categories, quiet hours, digest,
muted spaces and the time zone, and a save made from a stale form reloads with a conflict notice.

## Administration settings

Administrators set the bot, the policy and the per-space settings through the API or the CLI (Terraform resources are planned; the web app
shows the bot's health on the System page only). They are
all described in [Telegram setup](/operators/telegram#administration-settings): the **bot** (token, mode, Mini App host),
**space** settings (above) and **limits**.

## Unlanded work

These parts of this page depend on branches that are approved but not in the base build. Control checks them against the
merged code before the page is final.

| Topic | Branch | Affects |
|---|---|---|
| Watch-only terminal in the Mini App | `feat/telegram-miniapp-terminal` (TG-10-3) | [The Mini App](#the-mini-app) |
| Topics mode | `feat/telegram-topics` (#107) | [Topics mode](#topics-mode) |
| Version checks on move, acknowledge, resolve | `feat/telegram-ifmatch` (#109) | [Queue, runs and messages](#queue-runs-and-messages) |
| Ordering seam for the Mini App host load | `test/telegram-miniapp-ordering` (#110) | Test-only: no change to what you see |
| Editable Mini App settings, queue Next card | `feat/telegram-miniapp-settings-edit` (#112) | [The Mini App](#the-mini-app), [Notifications](#notifications-quiet-hours-and-digest) |
| Group mode | `feat/telegram-group-authz` (TG-13-1); binding is TG-13-2 | [Group mode](#group-mode) |
