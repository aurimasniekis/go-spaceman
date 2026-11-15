# Plan: Telegram chat UX — a thread per space, and a cross-space "Needs you" in the Mini App

Status: **proposed** (planner, 2026-11-15). Base: `next/v0.1.6` (e357e52c). No code has been changed for it.
Builds on ADR-0083 (Telegram), decision 16 (topics mode) and TG-11 (#107). It accounts for the topics-settings fix
that is running in parallel (`TopicsPrefs` read-only state in `internal/telegram/link/prefs.go` and
`web/src/tg/pages/TgSettings.tsx`).

Operator feedback (2026-11-15): the chat UI is messy. The operator asks for a thread per space carrying everything
that needs action, and a "Needs you" entry on the Mini App's home page that works across spaces, like the web.

File references are to `next/v0.1.6`. All Bot API facts are from the documentation, never from a real bot, unless a
line says otherwise (see §6.3).

---

## 1. Today's chat UX

### 1.1 What a linked user receives in the private chat

| What | Built by | Where it lands today |
|---|---|---|
| Needs-you card (an open human action: question, approval, to-do, message, custom) | `notify.StoreReader.action` (`internal/telegram/notify/store.go:453-504`), `Notifier.enqueue` (`notify/notify.go:515-549`) | Main chat. In topics mode: one shared **"Needs you"** topic for every space (`topics.Route(space, needsYou=true)` returns `topics.Needs`, `topics/topics.go:77-86`) |
| Inbox item, task in review, message to the operators (including control's replies), failed or lost run | `store.go:428-433, 506-530` | Main chat. In topics mode: the space's own topic |
| Reminder / "Escalated" / "Assigned to you" alert: a separate message with the title only | `Notifier.announce` / `enqueueAlert` (`notify.go:416-455`) | Like a needs-you card (the Needs-you topic in topics mode) |
| Daily digest | `notify/schedule.go:230-294`, sent with a plain `c.SendMessage` (`:251`) | Always the main chat |
| "Many changes at once" notice, sender overflow notice | `notify.go:588-609`, `sender/sender.go:247-256` | Always the main chat |
| Link, unlink, reconfirm and slow-down notices | `link/link.go:457`, `link/unlink.go:106`, `updates/dispatch.go:427-581`, through `app/telegram.go:105-117` | Always the main chat |
| Command replies, `/needs` lists, forms, capture acknowledgements | `chat` package, `Handler.send` / `show` (`chat/chat.go:245-316`) | The thread the update came from (`withThread`/`threadOf`, `chat.go:160-165, 253-256`); the main chat if that topic is gone |

There are no run notices for runs starting or ending. Only failed or lost runs are notified.

### 1.2 What a card looks like

- **Notifier card** (`renderCard`, `notify/cards.go:280-295`): `**<Label>: <Title>**`, then a state line, then the
  body (`full` content level, up to 600 characters) or "Open the app for details."
  - It has **no buttons** (`upsert` sends and edits without `ReplyMarkup`, `cards.go:237-259`).
  - It **doesn't name its space**.
  - A later state, including the final one, edits the same message in place (one card per chat and resource; the
    `telegram_messages` mapping, `migrations/0051_telegram.sql:107-125`, has no thread column).
- **Acting on an item** goes through `/needs` (`chat/needsyou.go:165-201`).
  - `/needs` is **per space** (it needs the right "current space"), even though `site/guide/telegram.md:98` says it
    works across spaces.
  - Each item opens as a **new** form message (`openAction`, `:362-388`) with its own buttons. Callback data is
    `h<verb>:<space>:<action>[:n]`, see `:145-161`.
  - Forms live in memory only (30 minutes, 10 per link; lost on restart).
  - Resolving an item elsewhere doesn't update an open form; a late tap gets "That request changed…".
- **Ready for card buttons:** a bare `ho` callback already resolves the action from the card mapping
  (`humanCallback`, `needsyou.go:288-306`; `Resolve`, `app/telegram.go:156-162`), but no card sends one. It appears
  only in tests.

### 1.3 Topics mode today (TG-11, #107)

- Switched on **per space** (`space_telegram_settings.topics`, `settings/space.go:30`; needs `space_config:write`
  plus reauth and MFA, `api/telegram_settings.go:18-26`). It is wired as `Enabled = sp.Topics && sp.Visible()`
  (`internal/app/central.go:1143`).
- A user can't choose. `topics_mode_default` (`settings/settings.go:69`) is stored but never read.
- Threads are kept per link in `telegram_links.topics_json` (`{spaceID|"needs": thread}`). Topics are created lazily
  with `createForumTopic`.
- A gone topic is recreated once, then the message falls back to the main chat. A refused create silences the chat for
  `RefusedFor` = 10 minutes (`topics.go:28, 118-167`).
- A message typed in a space topic is pinned to that space (`SpaceOf`, `topics.go:211-227`, used by `chat.go:171-177`).
- The bot reads `getMe` (`tgbot/runtime.go:464`), but ignores `has_topics_enabled` (`botapi/types.go:25`).
- Topic names are never updated when a space is renamed (there is no `editForumTopic` in `botapi`).
- A relink gets a new link ID, so it creates **duplicate topics** (`Forget`, `app/telegram.go:57-60`).

### 1.4 Why it feels messy

1. **Everything is interleaved.** Without topics, cards, alerts, digests, notices, command replies and capture acks
   from every space share one chat, and the cards don't say which space they belong to.
2. **Topics mode splits by kind, not by space.** Every space's open actions sit together in one "Needs you" topic,
   still without the space name. The other kinds of the same space are in the space's topic, and spaces without the
   flag stay in the main chat. An operator has to turn it on for each space; a user can't.
3. **Several messages per item.** One action can produce:
   - the card;
   - one alert per reminder, escalation or assignment;
   - a `/needs` list;
   - a fresh form each time it's opened;
   - a re-sent form when an edit fails (`needsyou.go:807-813`);
   - ForceReply prompts.
4. **Cards can't be acted on.** The user has to go through `/needs`, which depends on the in-memory current space.
   `telegram_links.current_space_id` exists but is never read or written (`chat/spaces.go:98-161`).
5. **Nothing is cleaned up.** Resolved-elsewhere forms keep live-looking buttons. Cards of muted spaces, or of spaces
   the link lost access to, stay at their old state (`notify.go:360, 369`). After the 30-day mapping retention, a
   still-open item can send a duplicate card.
6. **Restarts orphan state:** forms, prompts and the current space.
7. **Docs drift:** groups get no notifier cards (the guide's line 185 says they do), `/needs` isn't cross-space, and
   `topics_mode_default` does nothing.

---

## 2. Target: one thread per space

### 2.1 What Telegram allows (docs; `docs/contracts/telegram.md` § Topics, to be extended in T1)

- Private-chat topics must be **enabled for the bot in BotFather**. No API call toggles it.
  - `getMe` reports `has_topics_enabled` (9.3) and `allows_users_to_create_topics` (9.4).
  - A BotFather Mini App setting can stop users from creating and deleting topics (9.4).
  - Nothing documents a per-user opt-in.
- Methods that work in private chats:
  - `createForumTopic` (9.4; name 1–128 characters, 6 icon colours);
  - `editForumTopic` (name 0–128 characters; no colour change);
  - `deleteForumTopic` (**deletes all of the topic's messages**);
  - `unpinAllForumTopicMessages`.
  - `close`/`reopen` and every `*GeneralForumTopic` method are documented for supergroups only.
- `message_thread_id` and `is_topic_message` are set on private-chat messages in topics (9.3). Topic links are
  `https://t.me/<bot>/<thread_id>` (10.4) and open only for that user.
- Editing has no time limit for the bot's own messages. `deleteMessage` works only within 48 h.
  `pinChatMessage` works for any non-service message in a private chat, with no thread parameter.
- Commands and the menu button are per chat, never per topic.
- **Not documented:**
  - whether a private chat has a "General" topic;
  - where a message sent **without** `message_thread_id` appears in a topics-enabled private chat;
  - whether users can rename the bot's topics;
  - the error text when topics are off;
  - whether service messages (`forum_topic_*`) arrive;
  - the BotFather setting's name.

  These go on the real-bot checklist (§6.3). The design doesn't depend on any of them, thanks to the fallbacks below.

### 2.2 Routing

**Thread mode is decided per link** (the Telegram user's own chat). It no longer depends on the space:

```
effective(link) = link.threads override (on|off)  ?? notification_defaults.threads (auto|on|off, default auto)
on              = effective == on, or effective == auto and bot.has_topics_enabled, and the chat isn't refused now
```

When `on`, **every message about a space** goes to that space's topic, created lazily:
- needs-you cards of every human-action type, with their reminders, escalations and assignments;
- inbox items, tasks in review, messages to operators and control's replies;
- failed or lost runs;
- capture acks and command replies for that space.

The separate "Needs you" topic is retired (§2.4).

**Home** is everything not tied to one space:
- the daily digest, overflow notices and link/unlink/reconfirm notices;
- the cross-space `/needs` list and `/spaces`;
- the pinned **home summary** (§2.5).

Home is the chat **without a `message_thread_id`** (key `""`, what the main chat is today). Where such messages appear
in clients is undocumented. If the real-bot check shows they are hidden or awkward, home becomes a bot-created "🏠 Home"
topic (key `home`). That is a one-line change in `Route` and the Manager already supports extra keys, so the plan keeps
it switchable (Q2).

Replies keep today's rule: a command or capture answers in the thread it came from (`chat.go:253`). A command typed in a
space topic acts on that space (`SpaceOf`, unchanged; it answers only from threads the bot created).

**Fallback** (thread mode off, the bot lacks topics, or the chat refused): everything goes to the main chat as today.
Cards there name their space in the header (§2.6), and the same buttons and editing in place apply. So the fallback is
still much cleaner than today.

**User-created topics** (allowed unless BotFather disables them) are unknown to `SpaceOf`. A message in one acts on the
current space as today. Recommend turning user topic creation off in BotFather (Q8). The bot's status shows both
`getMe` flags (T1).

**Authorization is unchanged.** Topics choose only where a message appears. Every send still passes `may()`/`stillMay()`
(`notify.go:460, 565`), and every tap still runs as the tapping link through the API.

### 2.3 Who turns it on, per user or per space

- **Per link** (recommended, Q1). It's the user's own chat layout, so no permission is involved beyond owning the link.
  - It is a new field `threads` in `NotificationDefaults` / `NotificationDefaultsInput` (`settings/settings.go:93`),
    so the existing override machinery (`LinkPrefs.Effective`, `settings/prefs.go:30`) gives:
    - the global default (system admin, `put-telegram-settings`, Terraform);
    - the per-link override (`set-my-telegram-link-prefs`, own link, Mini App settings).
  - No SQL migration: both live in JSON columns (`telegram_settings.settings_json`, `telegram_links.prefs_json`).
- **The per-space `topics` flag is deprecated.** Routing ignores it. The API and Terraform keep accepting and returning
  it (documented as deprecated, "no effect"), and it is removed in a later release with a DECISIONS entry. A space that
  should never appear in Telegram is still hidden by its visibility, and a user mutes a space with `muted_spaces`.
- **`topics_mode_default` is deprecated** in the same way, replaced by `notification_defaults.threads`.
- The read-only `TopicsPrefs` added by the parallel fix (`available`, `spaces`, `refused`) is extended, not replaced:
  - `available` = the bot has topics enabled;
  - `mode` = the effective value;
  - `override`;
  - `spaces` = the spaces that currently have a topic in this chat (readable ones only, as the fix already filters);
  - `refused`.

  T3 must start from the fix's merged shape.

### 2.4 Migration of existing chats and settings

All of this happens at runtime and is idempotent. No schema change is needed.

1. **Links of spaces that had the flag** keep their space topics (same `topics_json` keys). New needs-you cards of those
   spaces now go to the space topic.
2. **The legacy `needs` thread** is handled the first time a link's topics load after upgrade:
   - the bot posts one note in it ("Needs-you items now arrive in each space's own topic. You can delete this topic.");
   - drops the `needs` key from `topics_json`;
   - never deletes it: `deleteForumTopic` would delete its messages (Q3).

   Cards already in it keep editing in place (an edit addresses only `chat_id` + `message_id`, `cards.go:239`), so open
   items still resolve visibly.
3. **Links that never had topics** (most of them) switch at their next space message when the effective mode is on.
   Old main-chat cards keep editing in place. One home message announces the change once per link, with a button to the
   Mini App settings to turn it off.
4. **Relink of the same chat and Telegram user** copies `topics_json` from the most recent revoked link instead of
   creating duplicate topics (T7). This is safe because `SpaceOf` only maps a thread to a space; authorization is still
   checked per tap.
5. **Rollback:** setting `notification_defaults.threads = off` returns every link that has no override to the main
   chat. **Expensive to undo:** topics already created in users' chats stay there (they can only be deleted together
   with their messages). That is the one irreversible effect. Say so in the release notes.

### 2.5 Home summary (pinned, edited in place)

One message per link in home, pinned once (`pinChatMessage`) and then only edited, debounced to at most one edit per
30 s per chat (edits are silent):

```
Needs you — 5
• Alpha — 3   ▸ (link to Alpha's topic)
• Beta — 2    ▸
[ Open Needs you ]   ← web_app button: Mini App /human
```

- Counts come from the notifier's own state (open human actions the link may see); no new query per recipient.
- It is rebuilt on each window that touches a human action of a space the link can read.
- With thread mode off, the per-space lines use the `space-<id>` start-param links instead of topic links.
- Its mapping is a `telegram_messages` row with `kind='home'` (the column already exists) so it survives restarts.
- Optional, Q5.

### 2.6 Message format

**Card layout** (one renderer shared by the notifier and the chat package; see T4):

```
❓ Question · Alpha            ← space name only outside the space's own topic
**Deploy the release candidate?**
Priority high · due in 2 h · from control
<body, full content level only, ≤ 600 characters>
[ Yes ] [ No ] [ ✏ Answer ]      ← quick actions by type
[ More… ]  [ Open in app ]
```

**Quick actions by type.** Explicit callback data in the existing `h<verb>:<space>:<action>` scheme (it fits in 64 bytes,
as forms already prove). It doesn't use the bare `ho` mapping lookup, because a mapping can be up to 1 s unflushed
(`cards.go:206`).

| Type | Buttons on the card |
|---|---|
| Question | its options (up to 6), then "✏ Answer" |
| To-do | "Mark completed" |
| Message | Acknowledge / Dismiss |
| Approval | Approve / Deny, with the existing two-step confirm. An enforced approval in a `web_only` space shows only "Decide in app" (ADR-0083 decision 14 unchanged) |
| Custom | "Open" (the form in place) or "Open in app" |

**More… turns the card itself into the full form**, edited in place:
- `openAction` targets the card's message ID instead of sending a new one;
- claim, comment, ask requester and files are available there;
- Back returns to the card.

No new form messages are created from a card. `/needs` lists open items as buttons that link to each card when it
exists, or open a card when it doesn't.

**Buttons follow the link.** The notifier knows the link's subjects in memory. It hides Approve/Deny when the link lacks
`approval:decide`, and hides the whole row for a `summary` group. A tap is still authorized by the API.

**Every edit carries the keyboard.** Telegram drops an inline keyboard on an edit without `reply_markup`, which needs a
real-bot check (§6.3). So the notifier always re-renders the text and keyboard together.

**Reminders, escalations and assignments** become **replies to the card** (`reply_parameters`), so they make a sound and
sit next to the item. The card's meta line also shows "reminded 2×" or "escalated". A newer alert deletes the previous
alert for the same card (within 48 h; best effort).

**After resolution**, the card is edited to a compact final form with no keyboard: `✅ Approved by Alice · Deploy the
release candidate?`. The body is dropped. It is not deleted (Q4).
- Alerts for the card are deleted (best effort, within 48 h).
- A form opened from the card collapses into the same compact line.
- When the link loses access, mutes the space, or the space is hidden, existing cards are edited to "No longer
  available here" instead of staying stale. This needs a final-state path that doesn't require `may()` but sends no
  content: today `notify.go:360, 369` skip them.

---

## 3. Mini App: a cross-space "Needs you" home

What exists:
- **The API is there.** `GET /api/v1/human-actions` (`list-pending-human-actions`, `internal/api/human.go:178-207`,
  rule `FilteredRead`) is already classified `TelegramBoth` (`internal/telegram/ops.go:15`).
  - It returns `{items, counts per space, total}`, filtered per space by `human:read` for the link's subjects (link cap
    ∩ user).
- **The web page already renders in the Mini App.** The web's cross-space list is `/human` ("Needs you",
  `web/src/routes/human.tsx`; nav `web/src/lib/nav.ts:21`). It renders inside `TgShell` today and
  `web/e2e/tg-needsyou.spec.ts:111-183` drives forms from it.
- **Live updates already work.** `/watch` invalidates `['human','search']` and `['human','pending']` on any
  human-action change (`web/src/api/watch.ts:179-183`).
- **Item kinds:** the web's "Needs you" is human actions only. Messages to operators, tasks and runs appear only as
  counts in `/attention`.

Missing pieces (T6):
1. **The home page doesn't link it.** `TgHome` (`web/src/tg/pages/TgHome.tsx:15-65`) shows Spaces and "Needs attention"
   (`/attention`) counts, active runs and the spaces list.
   - Add a **Needs you** section at the top: the total (`useHumanPending`), the 5 oldest or most urgent items as
     `ActionList` rows with their space names, and a "See all" button to `/human`.
   - Change the "Needs attention" card to secondary.
2. **No deep link to it.** Add kind `needs` (no space) to `web/src/tg/deeplink.ts:13-35` → `/human`. The bot's home
   summary uses `AppPage("/human")` in private chats and `?startapp=needs` where `web_app` buttons aren't allowed.
3. **Phone tailoring of `/human`:** in the Mini App, collapse `ActionFilters` behind a "Filter" button and keep the
   "reminded or escalated" toggle. Everything else is shared.
4. **`/attention` in the Mini App** links to web-only pages and uses `SpaceCounts` without `miniApp`
   (`features/attention/AttentionView.tsx:71, 137-158`). Pass `miniApp` and hide the installation links. This is a small
   fix found on the way.
5. No server change is needed for parity with the web. Showing more kinds in one list (operator messages, tasks in
   review, failed runs) would need a new or extended operation, a classification in `ops.go` and a security review
   (`ops.go:6-9`) (Q7, default no).

---

## 4. Tasks

Tiers: **hard** = implementor-hard, **medium**, **light**. Every task updates its own tests and the docs it touches,
runs `make check-fast` while iterating and `make check` once before pushing (plus `make web-check` / `make e2e` when
`web/` changes). It targets `next/v0.1.6`.

```
T0 ─┬─> T1 ─┬─> T2 ─┬─> T3
    │       │       ├─> T4 ─┬─> T5
    │       │       │       └─> T9
    │       │       └─> T7
    │       └─────────────────> T5
T6 (independent, can start now)          T8 docs (after T2–T7; the guide in each task, final pass here)
```

### T0 — ADR: chat threads per link (light, docs only)
- **Goal:** record the change to ADR-0083 decision 16 and Q6:
  - per-link `threads` (auto/on/off, default auto);
  - space flag and `topics_mode_default` deprecated;
  - legacy "Needs you" topic retired;
  - actionable cards edited in place;
  - home summary.
- **Files:** `docs/DECISIONS.md` (new ADR; ask control for the number), `docs/GOAL.md` (SM-TG-008/009 wording if
  needed), `docs/TRACEABILITY.md`.
- **Security label:** not a security-relevant change. Authorization is unchanged and topics are placement only. The ADR
  says so explicitly and lists the card-button callback path for security review in T4.
- **Acceptance:** the ADR records the operator's answers to §5. The requirement IDs are unchanged.
- **Depends on:** the operator's answers.

### T1 — Bot API surface and fake (light)
- **Goal:** the methods and fields the design needs, with no behaviour change.
- **Files:**
  - `internal/telegram/botapi/{methods,types}.go`: `EditForumTopic`, `PinChatMessage`, `UnpinChatMessage`,
    `DeleteMessages`, `User.AllowsUsersToCreateTopics`, `ReplyParameters` on sends (exists for chat; check the notifier
    path).
  - `internal/telegram/tgtest/{methods,topics,server}.go`: the same methods, a `getMe` topics toggle, pinned state,
    and recording which thread every message landed in (helper `MessagesIn(thread)`).
  - `tgbot/runtime.go:464`: keep `has_topics_enabled` and `allows_users_to_create_topics` in runtime state and expose
    them in the bot status (the `get-telegram-bot` output and the admin page that shows bot status).
  - `docs/contracts/telegram.md` § Topics: add the facts from §2.1 with URLs, and list the undocumented points.
- **Acceptance:**
  - client tests per method against the fake;
  - `openapi.json` and `schema.gen.ts` regenerated;
  - `TestTelegramClassificationIsExhaustive` stays green;
  - the contract file cites a source for every new fact.
- **Depends on:** T0 (it can start in parallel with T0, since it adds only surface).

### T2 — Threads core: per-link mode and routing (**hard**)
- **Goal:** §2.2–§2.4.
- **Settings:** add `Threads string` (`auto|on|off`, default `auto`) to `NotificationDefaults` and the input struct
  (`settings/settings.go:93-124`), with validation. Deprecate `TopicsModeDefault` (stored and ignored, with a doc
  string) and the space `Topics` flag (`settings/space.go:30`: routing stops reading it).
- **Manager** (`internal/telegram/topics/topics.go`):
  - `Enabled` becomes per link: `Enabled(linkID string, chatID int64) bool`, computed from the link's effective prefs,
    the bot's `has_topics_enabled` (T1) and `isRefused`.
  - `Route(linkID, spaceID)` returns `spaceID` for every kind and `""` (home) for cross-space messages.
  - Remove `Needs` from routing. Add `retireLegacy` on first load: a note in the `needs` thread, then drop the key.
  - Keep the `home` key switchable (a constant) for Q2.
- **Notifier:**
  - `notify.go:553-561`: `n.sender` routes by link, and alerts go to the space topic.
  - `schedule.go:251` and `notify.go:603`: the digest and overflow go through `Route(link, "")`, so they follow the home
    choice.
  - `app/central.go:1141-1152`: wiring.
- **Announcement:** one home announcement per link on switching (kept as a `telegram_messages` row of `kind='notice'`,
  so it is sent at most once).
- **Tests:**
  - unit tests in `topics_test.go`: per-link enable, `auto` follows `getMe`, the legacy `needs` retirement is
    idempotent and safe under concurrent sends, refusal fallback;
  - `notify_test.go`: action, alert, inbox, task, message and run all land in the space thread, and the digest lands
    in home;
  - update `internal/app/telegram_topics_e2e_test.go`: two spaces and one link; each kind lands in its space's topic;
    an old main-chat card still edits in place; `threads=off` puts everything in the main chat; with `getMe` topics
    off, `auto` uses the main chat.
- **Acceptance:**
  - no needs-you message is sent to a `needs` thread after the upgrade;
  - authorization tests unchanged and green (`telegram_security_e2e_test.go`);
  - `SpaceOf` still answers only from bot-created threads;
  - no per-recipient DB reads added in the notifier window (the ADR-0078 invariant).
- **Risks:** the notifier and Manager locking (`linkState.mu` held across create); a link's prefs changing mid-window
  (read the snapshot at send time, like `stillMay`); the topic-create storm when thousands of links switch at once (the
  sender's global 25/s bucket applies, since topic creation runs inside the queued send).
- **Depends on:** T1, T0.

### T3 — Thread mode in prefs and the Mini App settings (medium)
- **Goal:** a user turns thread mode on or off for their chat.
- **Files:**
  - `internal/telegram/link/prefs.go`: extend `TopicsPrefs` from the parallel fix with `mode` and `override`; save the
    override through the existing `threads` field of the overrides.
  - `api/openapi.json` and `web/src/api/schema.gen.ts`.
  - `web/src/tg/pages/TgSettings.tsx`: a "Chat layout: a topic per space / one chat / automatic" control, the refused
    and unavailable hints (including the BotFather hint the fix adds).
  - `web/e2e/tg-pages.spec.ts`.
  - The web account page's Telegram card, if it shows link prefs (`web/src/features/telegram/`).
- **Acceptance:**
  - the switch round-trips with If-Match;
  - a change reaches the notifier at once (`publishAfterCommit`);
  - Playwright covers the three states and axe stays clean.
- **Depends on:** T2, and the parallel topics-settings fix merged (rebase on it; the two touch the same files).

### T4 — Actionable cards edited in place (**hard**)
- **Goal:** §2.6.
- **Shared renderer:** a new package `internal/telegram/card` (pure, no I/O) that builds the text, entities and inline
  keyboard from a resource view plus the link's capabilities. It is used by `notify/cards.go` (`renderCard`, `upsert`:
  send and edit always carry the keyboard) and by `chat/needsyou.go` (`formHead`, `formViewBase`).
- **Card buttons** use the explicit `h<verb>:` callback data. "More…" makes `openAction` adopt the card's message ID
  (`show` edits, never sends). Back re-renders the card.
- **Notifier:**
  - Resolve reads the in-memory card mapping before the DB (removes the ≤1 s unflushed window for `ho` taps that
    already exist in tests).
  - The final compact form; "No longer available here" edits for lost access, mute or hidden space (rendering no
    content).
  - Alerts as replies to the card, with the previous alert deleted.
- **Space name** in the header outside the space's own topic.
- **Tests:**
  - golden renderer tests per type and content level;
  - `needsyou_test.go` / `needsyou_actions_test.go`: a tap on a card answers, the card collapses, a second tap gets
    the conflict text;
  - `notify_test.go`: edits carry the keyboard, the final state has none;
  - an e2e in `internal/app/telegram_needsyou_e2e_test.go`: resolve on the web and the card collapses in chat.
- **Security review** (in the PR):
  - the callback path still authorizes as the tapping link (`humanCallback` group/link checks, `needsyou.go:300`);
  - hidden buttons are cosmetic only;
  - an enforced approval stays web-only.
- **Acceptance:**
  - one message per item per chat (no new form messages from a card);
  - every type answerable from the card or its in-place form;
  - all existing chat-form tests pass or are updated to the in-place form.
- **Risks:** this touches the largest files (`needsyou.go` at 1137 lines, `notify.go`). The notifier and chat handler
  edit the same message, so they need ordering: chat edits go through `show`, and the notifier's edit after a version
  bump must win, because the final state is authoritative. Pin this down with a test of the race.
- **Depends on:** T2 (both touch `notify.go`; do them in sequence).

### T5 — Home: cross-space `/needs` and the pinned summary (medium)
- **Goal:** §2.5, plus a cross-space `/needs`.
- **Files:**
  - `chat/needsyou.go` `cmdNeedsYou`: in home (no pinned space), list across spaces from `GET /human-actions`, grouped
    by space, with a "See all" `web_app` button to `/human`. In a space topic: that space only (as today).
  - `chat/spaces.go`: `/spaces` rows get a "Go to topic" URL button (`t.me/<bot>/<thread>`) when a topic exists.
  - The notifier: a home-summary builder (counts from memory, debounced edit, pinned once, `kind='home'` mapping).
  - `commands/commands.go` help text.
- **Tests:** chat tests for both `/needs` scopes; notifier tests for debounce, pin-once and restart (the mapping is
  reloaded); an e2e that the summary updates when an action resolves.
- **Acceptance:** `/needs` in home works without a current space; the summary never shows a space the link can't read
  (tested with a capped link).
- **Depends on:** T2, T4 (shared renderer), T1 (pin), T6 (the `needs` deep link, for the non-`web_app` fallback).

### T6 — Mini App home "Needs you" (light; can start now)
- **Goal:** §3, items 1–4.
- **Files:**
  - `web/src/tg/pages/TgHome.tsx`;
  - `web/src/tg/deeplink.ts` and `deeplink.test.ts` (kind `needs`);
  - `web/src/routes/human.tsx` (a Mini App branch: collapsible filters);
  - `web/src/features/attention/AttentionView.tsx` (`miniApp`, hide installation links);
  - `web/e2e/tg-pages.spec.ts` (home shows the total and the top items, "See all" opens `/human`);
  - `web/e2e/tg-miniapp.spec.ts` (`startapp=needs`);
  - a new `TgHome` vitest.
- **Acceptance:**
  - from a cold Mini App open, the home shows the cross-space count and items and reaches `/human` in one tap;
  - the count updates live through `/watch`;
  - no new API operation;
  - `make web-check` and the tg Playwright specs are green with axe clean.
- **Depends on:** none.

### T7 — Topic hygiene (light)
- **Goal:** keep names right and avoid duplicates.
- **Files:**
  - a space rename calls `editForumTopic` for every link with that space's thread (the notifier follows
    `space` changes, or the name is compared at the next send; pick the cheaper option, with no per-recipient DB read);
  - on relink of the same `chat_id` and Telegram user, copy `topics_json` from the latest revoked link
    (`internal/telegram/link/link.go`, link-reuse path, migration 0053);
  - store the topic name in memory to skip no-op edits.
- **Tests:** unit tests plus an e2e for rename and relink (no second `createForumTopic` in the fake).
- **Depends on:** T1, T2.

### T8 — Docs (light)
- **Goal:** users and admins understand the new layout.
- **Files:**
  - `site/guide/telegram.md`: rewrite "Topics mode" as "Chat layout"; fix the three drifts in §1.4 item 7; add BotFather
    setup (enable topics; disallow user topics) to the admin section and the bot setup guide.
  - `CHANGELOG.md`, including the irreversible-topics note from §2.4 item 5.
  - The provider docs for the deprecated `topics` / `topics_mode_default` attributes, if the provider exposes them
    (it doesn't today; check at the time).
  - `docs/TRACEABILITY.md`.
- **Acceptance:** `make site-check` is green and the guide matches the behaviour of T2–T7.
- **Depends on:** T2–T7.

### T9 — Integration e2e and the real-bot checklist (medium)
- **Goal:** one end-to-end scenario on the fake, and the list for the opt-in real run (TG-14).
- **Files:**
  - `internal/app/telegram_threads_e2e_test.go`: a link with three spaces, each kind landing in its space thread; the
    legacy `needs` topic retired; a resolved item collapsing; the home summary counting; fallback with topics off; a
    relink without duplicates.
  - A checklist in `docs/contracts/telegram.md`: the undocumented points of §2.1, keyboard removal on an edit without
    `reply_markup`, pins, and topic links.
- **Acceptance:** the e2e is green under race. The checklist lists each fact with how to observe it. Statuses stay
  "tested (simulated)" until the real run.
- **Depends on:** T4, T5, T7.

**Parallelism:**
- Now: T0 and T6.
- Then: T1.
- Then T2.
- Then T3, T4 and T7 in parallel. T4 and T7 both touch notify; T7 is small, so merge it first.
- Then T5.
- Then T9 and T8.

---

## 5. Open product questions (operator)

| # | Question | Recommended default |
|---|---|---|
| Q0 | ADR number for T0 (control) | the next free one at the time |
| Q1 | Thread mode per user (link) or per space? | **Per link**, default `auto` (on when the bot has topics). The space flag and `topics_mode_default` are deprecated with no effect, removed later |
| Q2 | Home: the chat without a topic, or a bot-created "🏠 Home" topic? | **The chat without a topic**, switchable to a Home topic if the real-bot check shows untargeted messages are awkward |
| Q3 | The legacy "Needs you" topic: leave it, or delete it (which deletes its messages)? | **Leave it**, post a note and stop using it |
| Q4 | A resolved card: compact edit, delete (only possible within 48 h), or keep as is? | **A compact one-line edit, no buttons**; never delete |
| Q5 | A pinned home summary edited in place? | **Yes**, debounced to at most one edit per 30 s |
| Q6 | Approve/Deny directly on the card? | **Yes** for non-enforced approvals, with the existing two-step confirm; enforced approvals stay as the space's setting says (`web_only` by default) |
| Q7 | Should the Mini App's "Needs you" also list operator messages, tasks in review and failed runs? | **No**: parity with the web's `/human` (human actions only). The rest stays in `/attention` and the space pages. More kinds would need a new API operation and a security review |
| Q8 | BotFather: enable private-chat topics, and stop users creating or deleting topics? | **Enable topics; disallow user topic creation** (operator action in BotFather; the bot status shows both flags) |
| Q9 | Reminders and escalations: a reply to the card (sound) or a silent card edit only? | **A reply to the card**, the previous alert deleted, and a "reminded 2×" line on the card |
| Q10 | Add run notices beyond failed or lost runs (for example "control finished")? | **No** for now; failed or lost runs only, as today |

---

## 6. Notes for implementors and reviewers

### 6.1 What must not change
- **Authorization:**
  - `may()`/`stillMay()` at send time;
  - taps authorized as the tapping link;
  - `SpaceOf` only from bot-created threads;
  - enforced approvals per ADR-0083 decision 14;
  - content levels (a `summary` space never shows a body; groups are always `summary`).
- **The notifier's ADR-0078 shape:** no fan-out writes, no per-recipient DB reads, mappings flushed in batches.
- **Groups (forum supergroups bound to a space)** keep their own routing (`chat/group.go`). This plan touches only the
  private chat.

### 6.2 Coordination with the parallel topics-settings fix
- It edits `link/prefs.go` (`TopicsPrefs{available, spaces, refused}`), `link/link.go` (`TopicSpaces`,
  `TopicsRefused`, `CanRead` options), `app/telegram.go` (`topicSpaces`), `topics.go` (`Refused`), `TgSettings.tsx`,
  the guide, and `tg-pages.spec.ts`.
- With per-link mode, `topicSpaces()` (which lists spaces with the flag on) has to change meaning: list the spaces that
  have a thread in this link's `topics_json`. Its `CanRead` filter stays. That change belongs to T3.
- `TopicsPrefs.Refused` and `Manager.Refused` are reused as they are.

### 6.3 Evidence
- Everything above is "tested (simulated)" at best until the opt-in real-bot run (TG-14).
- These facts the design leans on are undocumented and must be observed:
  - where untargeted messages appear;
  - whether the General topic exists;
  - keyboard removal on an edit without markup;
  - users renaming bot topics;
  - the error text when topics are off.
- The design has a fallback for each.
