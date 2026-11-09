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
 * Reloads the page once to pick up the current build. Returns false, without reloading, when the page was already
 * reloaded for this within the last minute (or the marker cannot be stored), so the caller shows a message instead.
 */
export function reloadOnce(now = Date.now(), reload: () => void = () => window.location.reload()): boolean {
  const last = lastReload()
  if (last && now - last >= 0 && now - last < reloadWindowMs) return false
  try {
    sessionStorage.setItem(marker, String(now))
  } catch {
    return false // without the marker a reload could loop
  }
  reload()
  return true
}

/**
 * Handles Vite's preload failures (a chunk's stylesheet or dependency did not download): reloads once and stops the
 * error, or lets it reach the route's error boundary when a reload already happened.
 */
export function installChunkReload(target: Window = window) {
  target.addEventListener('vite:preloadError', (event) => {
    if (reloadOnce()) event.preventDefault()
  })
}
