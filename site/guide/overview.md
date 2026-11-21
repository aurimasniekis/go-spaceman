# Overview

**Overview** is the first page of the web UI (`/`). It answers two questions before anything else: what needs a person,
and how much the installation is using. From top to bottom:

1. **Stat tiles.** Spaces (with the paused count), active runs (with the working count) and, for a viewer
   with global access, nodes (connected of all).
2. **Needs attention.** The five most severe items from the [Attention](/guide/attention) list, act-now items first. Each
   links to the place that handles it. **See all (N)** opens the full list.
3. **Usage.** Tokens today and over the last seven days, with the change against the seven days before (left out when
   that earlier week had no usage), a 14-day sparkline, and the top five spaces and the top five models by tokens over
   the last seven days. **Open Usage** opens the [Usage page](/guide/usage-page). The summary shows tokens only, without
   a cost estimate.
4. **Accounts.** One compact row per subscription account: its 5-hour and 7-day usage windows as bars, and any open
   usage limit. The strip is hidden when no account is bound to a space you can read.
5. **Active runs**, the **Spaces** table (with a "tok 7 d" column) and the **Nodes** table.

Each part shows only what you may read, with the same rules as the Attention and Usage pages: a viewer sees the spaces
they have a grant in and nothing of the others. The Telegram Mini App's home is separate and is not changed by this page.

<Shot name="overview" alt="The Overview page: the top attention items, the last seven days' tokens with their change, a 14-day sparkline, the top spaces and models, and the accounts strip with their windows" caption="The Overview page" />

The usage figures come from the usage ledger, which records Claude Code usage. Runs whose usage is not in the ledger
(Codex, and runs that only have Harbor Pilot figures) are not counted; the [Usage page](/guide/usage-page) lists them.
