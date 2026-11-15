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

// @mantine/hooks use-focus-trap queues an uncleared 0 ms setTimeout that can fire after jsdom has
// been torn down, throwing "window is not defined" (seen from UsageLimitsCard.test.tsx). Yield once
// on a real timer before teardown so that callback runs while the environment still exists. The
// setTimeout is captured at module load, so the flush cannot hang when a test enables fake timers.
const realSetTimeout = globalThis.setTimeout
afterAll(() => new Promise((resolve) => realSetTimeout(resolve, 0)))
