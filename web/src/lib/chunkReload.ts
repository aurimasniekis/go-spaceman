/**
 * Recovery from a part of the app that failed to download. Routes are split into chunks with hashed names that are
 * fetched on first navigation. A tab opened before an upgrade still asks for the old names, which the new build no
 * longer has (404), and a dropped connection fails the same way. The browser remembers a failed module for the life of
 * the page, so only a reload helps: it picks up the current index.html and its chunk names.
 *
 * TanStack Router reloads by itself only for some messages and remembers each message for the whole tab session, so it
 * misses Vite's "Unable to preload CSS" and, on Safari (every iOS browser), whose message names no URL, every failure
 * after the first. Here one reload is allowed per window of time, so a later upgrade in the same tab recovers again
 * while a build that keeps failing cannot loop.
 */

const marker = 'spaceman:chunk-reload'
/** The prefix of TanStack Router's per-message reload marker (lazyRouteComponent in @tanstack/react-router). */
const tanstackMarker = 'tanstack_router_reload:'
let pending = false

/**
 * Reports whether this page has asked for a reload that has not landed yet. Until it does, more errors follow from the
 * same missing chunk (a route whose import failed, a component that never arrived), and the error screen shows the
 * loader for all of them instead of a red alert that disappears a moment later.
 */
export function reloadPending(): boolean {
  return pending
}
/** A second failure within this window after an automatic reload is shown instead of reloading again. */
export const reloadWindowMs = 60_000

const patterns = [
  /Failed to fetch dynamically imported module/i, // Chromium
  /error loading dynamically imported module/i, // Firefox
  /Importing a module script failed/i, // Safari and every iOS browser
  /Unable to preload CSS/i, // Vite's preload helper
  /is not a valid JavaScript MIME type/i, // an HTML page served for a module
  /Loading (CSS )?chunk .* failed/i,
]

/** Reports whether an error means a part of the app (a script or stylesheet chunk) could not be downloaded. */
export function isChunkLoadError(e: unknown): boolean {
  if (!e || typeof e !== 'object') return false
  const { name, message } = e as { name?: unknown; message?: unknown }
  if (name === 'ChunkLoadError') return true
  return typeof message === 'string' && patterns.some((p) => p.test(message))
}

function lastReload(): number {
  try {
    return Number(sessionStorage.getItem(marker)) || 0
  } catch {
    return 0
  }
}

/**
 * Tells TanStack Router's lazy route component that this failure is handled: it would otherwise call reload() again for
 * it, and Chromium then starts the navigation a second time. A page made of several lazy routes (/spaces is spaces.tsx
 * and spaces.index.tsx) fails once per chunk, each with its own message.
 */
function markHandled(cause: unknown) {
  const message = (cause as { message?: unknown } | undefined)?.message
  if (typeof message === 'string') sessionStorage.setItem(`${tanstackMarker}${message}`, '1')
}

/**
 * Reloads the page once to pick up the current build. Returns false, without reloading, when the page was already
 * reloaded for this within the last minute (or the marker cannot be stored), so the caller shows a message instead.
 * While this page's reload is pending it returns true without reloading again, and marks `cause` as handled.
 */
export function reloadOnce(now = Date.now(), reload: () => void = () => window.location.reload(), cause?: unknown): boolean {
  try {
    if (pending) {
      markHandled(cause)
      return true
    }
    const last = lastReload()
    if (last && now - last >= 0 && now - last < reloadWindowMs) return false
    sessionStorage.setItem(marker, String(now))
    markHandled(cause)
  } catch {
    return false // without the marker a reload could loop
  }
  pending = true
  reload()
  return true
}

/**
 * Handles Vite's preload failures: a chunk or its stylesheet did not download (also for a hover preload or an import
 * outside the router). Vite raises the same event for a chunk that downloaded but threw while it ran; that is a bug to
 * show, not a reason to reload, so only download failures reload.
 *
 * The error is not stopped (no preventDefault): stopped, Vite resolves the import with undefined, or runs the chunk
 * without its stylesheet, and the code waiting for the module fails with an unrelated TypeError. Left alone, the real
 * error reaches the route's error screen, which shows the loader while the reload is pending, or "SpaceMan was
 * updated" when a reload already happened and did not help.
 */
export function installChunkReload(target: Window = window) {
  target.addEventListener('vite:preloadError', (event) => {
    const { payload } = event as Event & { payload?: unknown }
    if (isChunkLoadError(payload)) reloadOnce(Date.now(), undefined, payload)
  })
}

/** Resets the pending reload; for tests, which keep one page across cases. */
export function resetChunkReload() {
  pending = false
}
