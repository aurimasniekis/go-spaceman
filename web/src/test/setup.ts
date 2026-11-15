import '@testing-library/jest-dom/vitest'

// jsdom has no Range geometry, and Element's getBoundingClientRect returns zeros while its
// getClientRects returns an empty array. ProseMirror (behind Tiptap) calls getClientRects /
// getBoundingClientRect from scrollIntoView and coordsAtPos; under load that surfaces as an
// unhandled "target.getClientRects is not a function" and fails the run. Provide empty geometry
// only where jsdom lacks it, so the real implementation wins if present.
const emptyRect = (): DOMRect =>
  ({
    x: 0,
    y: 0,
    top: 0,
    right: 0,
    bottom: 0,
    left: 0,
    width: 0,
    height: 0,
    toJSON() {
      return this
    },
  }) as DOMRect

const emptyRectList = (): DOMRectList =>
  Object.assign([] as DOMRect[], { item: () => null }) as unknown as DOMRectList

// Node-environment test files have no DOM globals, so only stub what exists.
const stubs: Array<[object | undefined, string, () => unknown]> = [
  [globalThis.Element?.prototype, 'getClientRects', emptyRectList],
  [globalThis.Element?.prototype, 'getBoundingClientRect', emptyRect],
  [globalThis.Range?.prototype, 'getClientRects', emptyRectList],
  [globalThis.Range?.prototype, 'getBoundingClientRect', emptyRect],
]

for (const [proto, name, make] of stubs) {
  if (proto && !(name in proto)) {
    Object.defineProperty(proto, name, { configurable: true, writable: true, value: make })
  }
}

// Mantine timers (use-focus-trap's 0 ms setTimeout, Transition's enter/exit timers) can outlive a test file and fire
// after jsdom has been torn down, throwing an unhandled "window is not defined" that fails the whole run (seen from
// UsageLimitsCard.test.tsx and TelegramLink.test.tsx, about one run in three). Track the timers a file schedules,
// give the ones due right away a real tick to run while the environment still exists, then clear what is left: no
// test cares about a timer that has not fired by the end of its file. The real setTimeout/clearTimeout are captured at
// module load, so this cannot hang or misfire when a test enables fake timers.
const realSetTimeout = globalThis.setTimeout
const realClearTimeout = globalThis.clearTimeout
const pending = new Set<ReturnType<typeof setTimeout>>()

globalThis.setTimeout = ((fn: (...a: unknown[]) => void, ms?: number, ...args: unknown[]) => {
  const id: ReturnType<typeof setTimeout> = realSetTimeout((...a: unknown[]) => {
    pending.delete(id)
    return fn(...a)
  }, ms, ...args)
  pending.add(id)
  return id
}) as typeof setTimeout

globalThis.clearTimeout = ((id?: ReturnType<typeof setTimeout>) => {
  if (id !== undefined) pending.delete(id)
  return realClearTimeout(id)
}) as typeof clearTimeout

afterAll(async () => {
  await new Promise((resolve) => realSetTimeout(resolve, 0))
  for (const id of pending) realClearTimeout(id)
  pending.clear()
})
