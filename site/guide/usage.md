# Usage and transcripts

SpaceMan keeps three records of a run apart:

- the **terminal recording**: what the run's terminal showed, for live view and replay;
- the harness's **native transcript**: the conversation as the harness itself saved it;
- **usage**: token counts and cost estimates SpaceMan reads from the harness's own records.

## Usage

<Shot name="run-usage" alt="Usage card of a worker run: claude-sonnet-5-5 with 2 requests and its input, output and cache token counts from the transcript, and a stored 1 KiB transcript with a download button" caption="A run's usage" />

- **Claude Code**: SpaceMan reads the run's session transcript and counts tokens per model: input, output, cache
  writes and cache reads. Claude Code also records its own **cost estimate**; SpaceMan shows it with what it assumes
  (API list prices; subscription accounts are not billed per token). SpaceMan has no price list of its own.
- **Codex**: its records are not verified yet, so its usage is **unknown**, unless the run went through the Harbor
  Pilot gateway (below).
- **Runs through [Harbor Pilot](/operators/harbor-pilot#budgets-and-usage)**: after the run ends, SpaceMan reads what
  the gateway recorded for it (requests, tokens per model, Harbor Pilot's cost) and shows it as **gateway-observed**.
  It becomes the run's usage, labelled "from Harbor Pilot", only when the harness reported none (Codex); Claude Code's
  own figures are kept and the gateway's are shown beside them. It is read a few times over the first 10 minutes after
  the end and is final after the last read.

Unknown usage is never shown as zero. While a run is going its usage is "so far"; it is final when the run ends.

<Shot name="space-usage" alt="Usage card on the space overview: known output and cache token totals, one run with known usage and five with unknown usage, with a note that totals cover known usage only" caption="A space's usage" />

The space overview adds up the runs whose usage is known and counts the others separately.

## Transcripts

When a run ends, its node uploads the harness's native transcript to SpaceMan, which checks it arrived intact and stores
it. People with permission to read recordings can download it from the run page, and delete it.

::: warning Transcripts may contain secrets
A transcript holds everything the session saw: file contents, command output, anything pasted. Treat it like a
recording.
:::
