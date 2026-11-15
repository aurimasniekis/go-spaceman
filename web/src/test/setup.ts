import '@testing-library/jest-dom/vitest'

// jsdom implements neither layout nor Range geometry. ProseMirror (behind Tiptap) calls
// getClientRects / getBoundingClientRect from scrollIntoView and coordsAtPos; under load
// that surfaces as an unhandled "target.getClientRects is not a function" and fails the run.
// Provide empty geometry only where jsdom lacks it, so the real implementation wins if present.
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

// Mantine transitions (Modal, Tooltip) schedule window timers that can outlive a test file. If one
// fires after jsdom is torn down it throws "window is not defined" and fails the run. Let pending
// transitions finish while the environment still exists (longer than Mantine's default durations).
afterAll(() => new Promise((resolve) => setTimeout(resolve, 300)))
