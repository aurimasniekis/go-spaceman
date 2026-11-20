# Attention

**Attention** (`/attention`) is the list of what needs a person. It is not in the sidebar: open it from the
[Overview](/guide/overview) (**See all**), from a space's overview, or from the Telegram Mini App. Each row names what the
item is, where it is, since when, and links to the run, task, queue, usage or cross-space page that handles it.

## Act now and Watch

The rows are grouped in two:

- **Act now**: someone has to do something. Blocked or failed tasks, quarantined events, runs whose state is unknown,
  account switches that failed in the last 24 hours, incoming cross-space requests, and budget notices that need action.
- **Watch**: the item may resolve by itself. Runs that failed in the last 24 hours, runs parked at a usage limit,
  open account limits, and budget warnings.

Pending human actions are one row, **Human actions**, that opens [Needs you](/guide/human-actions). The Act now count
includes them.

<Shot name="attention" alt="The Attention page: two spaces with pending human actions and quarantined events, outbound events, backups and central's storage with the data disk's free space" caption="The Attention page" />

## Filters and limits

- Filter by space and by kind. A filter that matches nothing says so.
- A page lists at most 20 items of each kind, the newest first. When a kind is cut, the page says how many there are in
  all: the full numbers are always counted. The counts are the same ones the space overviews show.
- A category appears only where you may read it. A viewer never sees another space's items.
- With `system:read`, the page also shows the installation: nodes that are not connected, outbound events that failed, backups,
  and central's storage. They are described on the [Observability](/operators/observability#attention-page) page.

The list is computed on request and refreshes when the change feed reports a change to a space, run, task, human action,
subscription, limit or backup. The CLI shows the same summary with `spaceman system attention`.
