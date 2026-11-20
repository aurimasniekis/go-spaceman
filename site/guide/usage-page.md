# The Usage page

**Usage** (`/usage`, in the sidebar's Work group) shows the tokens SpaceMan recorded across every space you can read.
The [per-run and per-space usage](/guide/usage) explains where the figures come from and what unknown usage means; this
page adds the view across spaces.

## What you see

- **Range**: 24 hours, 7 days, 30 days or 90 days. The 90-day range uses daily buckets.
- **Group by**: space, account, model, role or harness. The chart and the table follow the grouping.
- **Filters**: select an account, model, harness or space in the table to filter by it. A selected row's toggle is named
  "Remove filter" with its value.
- **Totals**: tokens (input and output, cache) and an **estimated cost**, labelled as an estimate.
- **Accounts**: each account bound to a space you can read, with its 5-hour and 7-day usage windows (used percentage and
  when they reset), its open usage limits and its tokens in the range. With an account filter on, accounts outside the
  filter show a dash.
- **Not counted**: the runs in the range whose usage is not in the ledger, counted per harness: Codex, and runs that only
  have Harbor Pilot figures. The note links to those runs. A missing figure is shown as unknown, not as zero.

The range, grouping and filters are in the page's address. A view can be linked to and survives a reload. A space row
opens that space's [Usage page](/guide/usage), which also offers the 90-day range.

## Who sees what

Anyone with `run:read` in at least one space can open the page. Rows and totals cover only the spaces you can read, and
an account's name appears only when it is bound to one of them, directly or through a role pool. The API and the CLI
follow the same rule.

<Shot name="usage-page" alt="The Usage page across spaces: tokens per day stacked by space, totals with the estimated cost, the not-counted runs note, the table and the accounts' usage windows" caption="Usage across the spaces you can read" />

## API and CLI

- `GET /usage/series` (`get-usage-series`) returns the series, with `group_by`, filters, `labels` and `not_counted`.
- `GET /account-windows` (`list-account-windows`) returns the windows and open limits of every account you can see.
- `spaceman usage all` and `spaceman usage windows` print the same data.
