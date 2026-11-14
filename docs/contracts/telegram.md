# Contract: Telegram Bot API and Mini Apps

Pinned to **Bot API 10.4 (2026-11-09)**, read in Telegram's public documentation on **2026-11-14**. The newest Mini App
entries are from Bot API 10.1. SpaceMan's design on top of it is ADR-0083 (Proposed, security-relevant change) in
`docs/DECISIONS.md`; the requirements are SM-TG-001…014 in `docs/GOAL.md`.

**How this was established:**
- The pages below were fetched on 2026-11-14 while planning the integration, and the facts SpaceMan relies on were
  spot-checked in the raw pages (`sendRichMessage`, `X-Telegram-Bot-Api-Secret-Token`, `sendMessageDraft`,
  business-only checklists, the 32768 rich-message limits).
- **No fact here has been observed against a real bot yet.** Each one is labelled **docs**: read in the documentation
  only. The fake Bot API server (`internal/telegram/tgtest`, TG-1) is built from this file; real verification is the
  opt-in `SPACEMAN_REAL_TELEGRAM=1` run of TG-14. Until it runs, every Telegram requirement stays "tested (simulated)" at
  best.
- Implementors must not invent API shapes (AGENTS.md). A fact SpaceMan needs that isn't recorded here is looked up in
  the documentation (or observed against a test-environment bot) and added here with its source and date first.

**Sources** (keys used below):

| Key | URL |
|---|---|
| API | https://core.telegram.org/bots/api |
| CL | https://core.telegram.org/bots/api-changelog |
| FEAT | https://core.telegram.org/bots/features |
| FAQ | https://core.telegram.org/bots/faq |
| WH | https://core.telegram.org/bots/webhooks |
| WA | https://core.telegram.org/bots/webapps |
| LINKS | https://core.telegram.org/api/links |
| LOGIN | https://core.telegram.org/widgets/login |
| SL | https://core.telegram.org/bots/serverless |
| B-WATCH | https://telegram.org/blog/watch-apps-and-more (2026-06-11) |
| B-COMM | https://telegram.org/blog/communities-editor-invisible-messages (2026-07-14) |
| B-AI | https://telegram.org/blog/ai-bot-revolution-11-new-features (2026-05-07) |

A citation such as `API#sendmessage` means the anchor `#sendmessage` on the API page.

## Getting updates

| Fact | Source |
|---|---|
| Webhook and `getUpdates` are mutually exclusive. Undelivered updates are kept for **at most 24 h**. | https://core.telegram.org/bots/api#getting-updates |
| `setWebhook`: an HTTPS `url` on port **443, 80, 88 or 8443**; `max_connections` 1–100 (default 40); `allowed_updates`; `drop_pending_updates`; `ip_address`. | https://core.telegram.org/bots/api#setwebhook |
| `setWebhook` `secret_token`: **1–256 characters of `[A-Za-z0-9_-]`**, sent back on every webhook request in the header **`X-Telegram-Bot-Api-Secret-Token`**. | https://core.telegram.org/bots/api#setwebhook |
| Telegram retries a webhook request that gets a non-2xx answer "a reasonable amount" of times (no number given). | https://core.telegram.org/bots/api#setwebhook |
| A webhook answer may carry a method call in its body, but the result of that call is then unknown. SpaceMan never uses it. | https://core.telegram.org/bots/api#making-requests-when-getting-updates |
| Webhooks are **IPv4 only**, need **TLS 1.2 or later** with the full chain, and come from **149.154.160.0/20 and 91.108.4.0/22**, which "might change". | https://core.telegram.org/bots/webhooks |
| `getUpdates`: long poll with `offset`, `limit` 1–100 and `timeout`; outbound HTTPS only. | https://core.telegram.org/bots/api#getupdates |
| `getWebhookInfo` reports `pending_update_count` and `last_error_date` / `last_error_message`. | https://core.telegram.org/bots/api#getwebhookinfo |
| `getMe` returns the bot's `id` and `username`, and `has_topics_enabled` (since 9.3). | https://core.telegram.org/bots/api#getme, https://core.telegram.org/bots/api-changelog#december-31-2025 |
| `allowed_updates` names the update kinds to receive; SpaceMan asks for `message`, `callback_query` and `my_chat_member` (TG-3a, `updates.AllowedUpdates`). | https://core.telegram.org/bots/api#update |
| `my_chat_member` (`ChatMemberUpdated`: `chat`, `from`, `date`, `old_chat_member`, `new_chat_member`) reports a change of the bot's own status in a chat; in a private chat only when the user blocks or unblocks the bot. A `ChatMember`'s `status` is `creator`, `administrator`, `member`, `restricted`, `left` or `kicked`. | https://core.telegram.org/bots/api#update, https://core.telegram.org/bots/api#chatmemberupdated, https://core.telegram.org/bots/api#chatmember |
| `leaveChat(chat_id)` makes the bot leave a group, supergroup or channel; it returns `True`. | https://core.telegram.org/bots/api#leavechat |

## Identity

| Fact | Source |
|---|---|
| Every update's `from.id` (message, callback query) is set by Telegram's servers; it is the identity the bot sees. | https://core.telegram.org/bots/api#user |
| Deep links `t.me/<bot>?start=<parameter>`: the parameter is **up to 64 base64url characters** (`[A-Za-z0-9_-]`) and arrives as `/start <parameter>`. A 43-character base64url code fits. | https://core.telegram.org/api/links#bot-links |
| Telegram Login is OpenID Connect: discovery at `https://oauth.telegram.org/.well-known/openid-configuration`, code flow with PKCE, client ID and secret from BotFather, `aud` = bot ID, RS256. Not used by SpaceMan (ADR-0083, rejected alternatives). | https://core.telegram.org/widgets/login |
| The legacy Login widget hash is `HMAC_SHA256(SHA256(bot_token), data_check_string)`. Not used. | https://core.telegram.org/widgets/login-legacy |

## Messages and formatting

| Fact | Source |
|---|---|
| Text may be formatted with explicit `entities` instead of `parse_mode` (`MarkdownV2`, `HTML`, `Markdown`). SpaceMan sends entities only, built from its own Markdown AST. | https://core.telegram.org/bots/api#formatting-options |
| Entity offsets and lengths are in **UTF-16 code units**. | https://core.telegram.org/bots/api#messageentity |
| Entity types include bold, italic, underline, strikethrough, spoiler, blockquote, `expandable_blockquote`, code, `pre` (with `language`), `text_link`, `date_time` (9.5), `bank_card_number` and `ton_address` (10.4). | https://core.telegram.org/bots/api#messageentity, https://core.telegram.org/bots/api-changelog#november-9-2026 |
| Overlapping entities must nest; `pre` and `code` can't contain other entities; blockquotes can't nest. | https://core.telegram.org/bots/api#formatting-options |
| Message text is **1–4096 characters after entity parsing**. | https://core.telegram.org/bots/api#sendmessage |
| Captions are **0–1024 characters**. | https://core.telegram.org/bots/api#sendphoto |
| `protect_content` stops forwarding and saving of a message (not screenshots). | https://core.telegram.org/bots/api#sendmessage |
| `disable_notification` sends silently. | https://core.telegram.org/bots/api#sendmessage |
| Rich messages: `sendRichMessage` with an `InputRichMessage` holding exactly one of `blocks`, `html` or `markdown`; limits **32768 UTF-8 characters, 500 blocks, 16 nesting levels, 50 media, 20 table columns**; clients show "Show More" after about 8000 characters. Bot API 10.1–10.3. | https://core.telegram.org/bots/api#sendrichmessage, https://core.telegram.org/bots/api#inputrichmessage, https://telegram.org/blog/watch-apps-and-more |
| `editMessageText` accepts `rich_message`. | https://core.telegram.org/bots/api#editmessagetext |
| Only messages without markup or with an inline keyboard can be edited. `deleteMessage` works only within 48 h. | https://core.telegram.org/bots/api#updating-messages, https://core.telegram.org/bots/api#deletemessage |
| Ephemeral messages: an ephemeral reply to a callback query works in any chat except channel direct-message chats (10.4); any bot may send one within 15 s of a callback or ephemeral command; delivery is not guaranteed. Used only in group mode (TG-13). | https://core.telegram.org/bots/api#ephemeral-messages-and-commands, https://core.telegram.org/bots/api-changelog#november-9-2026, https://telegram.org/blog/communities-editor-invisible-messages |
| Native checklists (`sendChecklist`) need a business connection, so SpaceMan can't use them. | https://core.telegram.org/bots/api#sendchecklist |

**Undocumented** (treated conservatively): the "message is not modified" error text; whether edits, deletes and drafts
count toward rate limits (SpaceMan counts them as sends).

## Keyboards, callbacks and commands

| Fact | Source |
|---|---|
| Inline buttons: `callback_data` is **1–64 bytes**; `url`; `web_app` (**private chats only**); `login_url`; `copy_text` (1–256 characters); `disabled` (10.3). No row or button maximum is documented. | https://core.telegram.org/bots/api#inlinekeyboardbutton |
| Buttons take `style` (`danger`, `success`, `primary`) and `icon_custom_emoji_id` since 9.4. | https://core.telegram.org/bots/api-changelog#february-9-2026 |
| `answerCallbackQuery` must always be called (the client shows a spinner until then); `text` 0–200 characters; optional `show_alert`. No deadline is documented. | https://core.telegram.org/bots/api#answercallbackquery |
| `ForceReply` with `input_field_placeholder` (1–64 characters). | https://core.telegram.org/bots/api#forcereply |
| `setMyCommands`: up to 100 commands, 7 scopes. | https://core.telegram.org/bots/api#setmycommands |
| `setChatMenuButton` with `MenuButtonWebApp`. | https://core.telegram.org/bots/api#setchatmenubutton |

## Topics

| Fact | Source |
|---|---|
| Messages in private chats carry `message_thread_id` since 9.3; bots can create topics in private chats since 9.4; topic links `https://t.me/<bot>/<thread_id>` since 10.4. Enabled in BotFather. | https://core.telegram.org/bots/api-changelog#december-31-2025, https://core.telegram.org/bots/api-changelog#february-9-2026, https://core.telegram.org/bots/api-changelog#november-9-2026, https://core.telegram.org/bots/features#topics-in-private-chats |
| Forum topics in supergroups: `createForumTopic` (name 1–128 characters). | https://core.telegram.org/bots/api#createforumtopic |

## Files

| Fact | Source |
|---|---|
| Sending by multipart upload: **10 MB for photos, 50 MB for other files**. By URL: 5 MB photos, 20 MB others. By `file_id`: no limit (file IDs are per bot). | https://core.telegram.org/bots/api#sending-files |
| Photos: width + height ≤ 10000, aspect ratio ≤ 20. | https://core.telegram.org/bots/api#sendphoto |
| `sendMediaGroup`: 2–10 items; documents only group with documents. | https://core.telegram.org/bots/api#sendmediagroup |
| `getFile` downloads are limited to **20 MB**; the link is valid for at least 1 hour; the original name and MIME type may not be kept. The file URL `https://api.telegram.org/file/bot<token>/<path>` contains the token. | https://core.telegram.org/bots/api#getfile, https://core.telegram.org/bots/faq#how-do-i-download-files |
| A local Bot API server lifts the limits (uploads to 2000 MB) but needs `api_id`/`api_hash` and a separate process. Not used. | https://core.telegram.org/bots/api#using-a-local-bot-api-server |

## Requests and answers

Used by the client `internal/telegram/botapi` and the fake `internal/telegram/tgtest` (TG-1). Read on 2026-11-14.

| Fact | Source |
|---|---|
| The token looks like `123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11`: a numeric bot ID, a colon, then letters, digits, `-` and `_`. The client refuses any other shape, so a token can't add path segments, a query or a fragment to a URL. | https://core.telegram.org/bots/api#authorizing-your-bot |
| Requests go to `https://api.telegram.org/bot<token>/METHOD_NAME`, by GET or POST, with parameters as a query string, `application/x-www-form-urlencoded`, `application/json` (not for uploads) or `multipart/form-data` (for uploads); UTF-8 only. | https://core.telegram.org/bots/api#making-requests |
| The answer is a JSON object with a Boolean `ok`; on success `result`, otherwise `description` and an `error_code` that is "subject to change"; some errors carry `parameters` (`ResponseParameters`: `retry_after`, `migrate_to_chat_id`). | https://core.telegram.org/bots/api#making-requests, https://core.telegram.org/bots/api#responseparameters |
| Uploads are multipart parts; a JSON field such as a media group's `media` refers to a part as `attach://<file_attach_name>`. | https://core.telegram.org/bots/api#sending-files, https://core.telegram.org/bots/api#inputmedia |
| `File` has `file_id`, `file_unique_id`, `file_size` and `file_path`; the download link is `https://api.telegram.org/file/bot<token>/<file_path>`, valid for at least 1 hour. | https://core.telegram.org/bots/api#file |
| `WebhookInfo` fields: `url`, `has_custom_certificate`, `pending_update_count`, `ip_address`, `last_error_date`, `last_error_message`, `last_synchronization_error_date`, `max_connections`, `allowed_updates`. | https://core.telegram.org/bots/api#webhookinfo |
| `getUpdates`: `offset`, `limit` (1–100, default 100), `timeout`, `allowed_updates`. An update is confirmed once `getUpdates` is called with an offset higher than its `update_id`; a negative offset counts from the end of the queue. | https://core.telegram.org/bots/api#getupdates |

**What the fake decides for itself** (not documented, so SpaceMan must not depend on it; real verification in TG-14
checks it):
- Lengths ("characters") are counted in UTF-16 code units, the stricter reading; an emoji counts as two.
- Error descriptions other than `Too Many Requests: retry after N`, `Forbidden: bot was blocked by the user`,
  `Unauthorized` and `Not Found` are the fake's own wording, all with the usual `Bad Request: ` prefix. Callers branch on
  the status code (`botapi.RetryAfter`, `botapi.IsForbidden`, `botapi.IsUnauthorized`), never on the text.
- `getUpdates` while a webhook is set answers 409; a wrong token answers 401; an unknown method answers 404.
- `leaveChat` on a private chat answers 400 (the documentation only says the method is for groups, supergroups and
  channels); SpaceMan never calls it for a private chat.
- Rate limits: 1 message/s per private chat with a burst of 3, 20/min per group, 30/s overall, as token buckets;
  sends, edits and deletes count; a media group counts as one send; `answerCallbackQuery` and reads don't count.
- `setWebhook` accepts only `https` URLs on ports 443, 80, 88 and 8443, as documented; tests deliver to their own
  `http://127.0.0.1` server through `tgtest.Options.WebhookTarget`.

## Rate limits

| Fact | Source |
|---|---|
| About **1 message/s per chat** (short bursts allowed), **20 messages/min per group**, about **30 messages/s overall**. | https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this |
| A 429 answer carries `parameters.retry_after` (seconds). | https://core.telegram.org/bots/api#responseparameters |
| A user who blocked the bot makes sends fail with 403. | https://core.telegram.org/bots/api#making-requests |

## Mini Apps

| Fact | Source |
|---|---|
| Launch modes: main Mini App, keyboard button, inline button, menu button, inline mode, direct link `t.me/<bot>/<app>?startapp=`, attachment menu. Keyboard-button and inline-mode launches get **empty `initData`**; inline-button, menu-button, main-app and direct-link launches get a signed `user`. | https://core.telegram.org/bots/webapps#implementing-mini-apps |
| `startapp` is at most 512 characters (changelog, 2023-08-18; the links page gives no maximum); its character set is undocumented. SpaceMan uses short ASCII routing hints only. | https://core.telegram.org/bots/api-changelog, https://core.telegram.org/api/links#direct-mini-app-links |
| `initData` fields: `query_id`, `chat_join_request_query_id` (10.1), `user`, `receiver`, `chat`, `chat_type`, `chat_instance`, `start_param`, `can_send_after`, `auth_date`, `hash`, `signature`. | https://core.telegram.org/bots/webapps#webappinitdata |
| **Validation:** the data-check-string is every field except `hash`, sorted, as `key=value`, joined with `\n`; `secret_key = HMAC_SHA256(key="WebAppData", msg=bot_token)`; valid when `hex(HMAC_SHA256(key=secret_key, msg=data_check_string)) == hash`. The docs suggest checking `auth_date` but give no maximum age. `initDataUnsafe` "should not be trusted". | https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app |
| Third parties can verify the Ed25519 `signature` with Telegram's published keys instead. Not used (SpaceMan holds the token). | https://core.telegram.org/bots/webapps#validating-data-for-third-party-use |
| Mini Apps need HTTPS. | https://core.telegram.org/bots/api#webappinfo |
| Telegram Web opens a Mini App **in an iframe**: "don't add X-Frame-Options or a restrictive frame-ancestors". The iframe's parent is `https://web.telegram.org`. | https://core.telegram.org/bots/serverless |
| **Since Bot API 10.2 (enforced from 2026-07-20), Mini App methods work only from the Mini App's own origin**; BotFather offers an opt-out, which SpaceMan's setup leaves off. | https://core.telegram.org/bots/api-changelog#july-14-2026 |
| The loader script is `https://telegram.org/js/telegram-web-app.js`. | https://core.telegram.org/bots/webapps#initializing-mini-apps |
| Theme: 15 `ThemeParams` keys (`bg_color` … `bottom_bar_bg_color`) mirrored as `--tg-theme-*` CSS variables; `colorScheme`; `themeChanged`; `setHeaderColor`, `setBackgroundColor`, `setBottomBarColor`. | https://core.telegram.org/bots/webapps#themeparams |
| Controls: `BackButton`, `MainButton`/`SecondaryButton`, `SettingsButton`, `HapticFeedback`, `showPopup` (1–3 buttons), `showConfirm`, `showAlert`, closing confirmation, safe-area insets, `requestFullscreen`, `openLink`, `close`. | https://core.telegram.org/bots/webapps |
| Storage: `CloudStorage` (1024 keys; key 1–128 characters; value up to 4096), `DeviceStorage` (5 MB, 9.0), `SecureStorage` (10 items, 9.0). SpaceMan keeps no credential in any of them. | https://core.telegram.org/bots/webapps |
| `downloadFile` (8.0): on Telegram Web the download is made from the `web.telegram.org` page, so the file URL needs `Access-Control-Allow-Origin: https://web.telegram.org`. | https://core.telegram.org/bots/webapps#downloadfileparams |

## Features SpaceMan does not use

Recorded so a later change knows what was considered (ADR-0083 decision 15):

| Feature | Source |
|---|---|
| Draft streaming `sendMessageDraft` (private chats only; a 30-second ephemeral preview; `can_stop` in 10.3) and `sendRichMessageDraft` (10.1). Later, not v1. | https://core.telegram.org/bots/api#sendmessagedraft, https://core.telegram.org/bots/api#sendrichmessagedraft |
| Guest bots, bot-to-bot messaging (needs a BotFather mode and loop prevention), managed bots (9.6), business bots. | https://core.telegram.org/bots/features#ai-agents, https://core.telegram.org/bots/features#bot-to-bot-communication, https://telegram.org/blog/ai-bot-revolution-11-new-features |
| HTML widgets `sendWidget` (10.4; private chats, ≤ 32 KB of HTML). | https://core.telegram.org/bots/api-changelog#november-9-2026 |
| Paid broadcasts (up to 1000 messages/s; the pages disagree on eligibility). | https://core.telegram.org/bots/api#paid-broadcasts |
| Polls (12 options, 100 characters: fewer than a SpaceMan question's 20 options). | https://core.telegram.org/bots/api#sendpoll |

## Third-party library

The Markdown parser for the renderer (TG-5) is **goldmark** (MIT, CommonMark plus GFM extensions,
https://github.com/yuin/goldmark). TG-5 records its pinned version and licence here and in `docs/THIRD_PARTY.md` when it
adds the dependency; Go has no Markdown library in `go.mod` at v0.1.4.
