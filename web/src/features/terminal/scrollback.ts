/**
 * How much earlier output a terminal keeps, in one place: createTerminal sets it, and anything that needs to know how
 * many rows a terminal can hold (a catch-up that writes only the tail) reads it here.
 *
 * The unit is **bytes**, not lines. ghostty-web 0.4.0's `scrollback` option goes unchanged into the WebAssembly
 * module's configuration as `scrollbackLimit`, and the module (0.4.0's and the 0.4.0-next.21 one we load, ADR-0076)
 * reads it as ghostty's `scrollback-limit`: the most memory, in bytes, that the pages of rows above the screen may
 * hold. Whole pages are dropped from the top once they pass it; 0 means no limit. The earlier `scrollback: 10_000`
 * kept about 1,100 rows at 80 columns and 290 to 470 at 160 (ADR-0080).
 *
 * A row costs about 13 bytes per column (its cells, plus the page's room for styles, graphemes and links), up to about
 * 14.5 when nearly every row has a colour of its own. Pages are dropped whole, so the rows kept rise and fall within a
 * page; with 24 MiB (measured, scrollback.test.ts):
 * - 160 columns (the node's default size): 12,090-12,380 rows of plain text, 10,900-11,200 in per-row 24-bit colours;
 * - 80 columns: 24,100-24,690 and 21,750-22,330;
 * - 200 columns: 9,670-9,900 and 8,720-8,960.
 * A terminal with a full scrollback of plain text, or of output in a few dozen styles, holds about 30 MiB of WebAssembly
 * memory (about 7 MiB with the old setting). Output whose lines each have a style of their own grows it by about
 * 3.5 MiB per 1,000 lines at 160 columns, whatever the limit (ADR-0080). Every terminal has its own instance, so its memory goes with it when it closes.
 */
export const SCROLLBACK_BYTES = 24 * 1024 * 1024

/** The most a row may cost per column, in bytes, rounded up from the measured 14.47 of per-row 24-bit colours. */
const BYTES_PER_CELL = 15

/**
 * The rows of output above the screen that a terminal of this width keeps at least: a conservative estimate from
 * SCROLLBACK_BYTES (10,485 at 160 columns). Rows wrapped from one long line count separately.
 */
export function scrollbackRows(cols: number): number {
  return Math.floor(SCROLLBACK_BYTES / (Math.max(1, cols) * BYTES_PER_CELL))
}
