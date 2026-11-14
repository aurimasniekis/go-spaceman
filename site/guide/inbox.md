# The inbox

Every space has an **inbox**: requests from people, alerts, events and messages that the space's control agent reads
and decides what to do with. An inbox item is **not** a task. Control may turn it into work, ask a person a question,
reply, associate it with something already in progress, defer it or reject it.

<Shot name="inbox-list" alt="Inbox of the Payments platform space with items in several states and urgencies" caption="A space's inbox" />

## Adding an item

Operators add items with **New item** on the Inbox page, or with **New inbox item** next to **Launch run** on the
space's overview or a run's terminal view (a toast then links to the new item). In the terminal view, **Send as inbox
item** next to **Send** starts the item from the message box's text and a link to the run. Write in the editor (bold, lists, task lists, tables, code blocks and links all
work), or switch the format to **JSON** for machine-readable input such as an alert payload. Paste or drop images and
files into the editor, or use the paperclip: images are placed inline, other files become links and are listed under the
editor. Press ⌘/Ctrl + Enter to add.

<Shot name="inbox-composer" alt="New inbox item dialog with formatted text, an attached CSV file and urgency set to high" caption="Writing an item with an attachment" />

The text is stored as Markdown, never HTML. Attachments are referenced as `spaceman://attachment/<id>`, which is also
how agents see them.

## Reading an item

The item page shows the rendered text, its attachments (images inline, other files as downloads), where it came from and
the **source metadata**: facts set by the authenticated source (for example the signed-in operator, the event
subscription, the schedule, the requesting space or the agent run), never taken from the text. Badges show when the
item was replayed, when it is a **human response** routed to control or the operators (made by a human-action policy,
[Responses in the inbox](/guide/human-actions#responses-in-the-inbox)), when control will be told about it only once
the paused space resumes (**control told on resume**, [While the space is paused](/guide/events#while-the-space-is-paused)),
and its automation **depth** when it came through other SpaceMan installations.

<Shot name="inbox-item" alt="Inbox item with formatted text, a task list, an inline screenshot and a log file attachment" caption="An item with an inline screenshot and a log file" />

Item text is **untrusted**: it may come from outside the team. It is rendered without scripts, styles or remote images,
links open in a new tab without a referrer, and an item never grants authority by itself.

Operators can mark an item in progress or done, defer it until a time, reject it with a reason, or reopen it. Each
change is checked against the version you saw, so two people cannot overwrite each other's decision, and every change is
audited.

## Attachments

- Uploads are limited to `http.max_upload_bytes` (25 MiB by default) each and `artifacts.space_quota_bytes` (1 GiB) per
  space.
- The type is detected from the content, not the file name. Only PNG, JPEG, GIF and WebP images are shown inline;
  everything else, including SVG and HTML, is always downloaded and can never run in SpaceMan's pages.
- Attachments belong to their space: they are only reachable by people who can read that space.
- Uploads that no item uses are removed after 24 hours. Identical files are stored once.
