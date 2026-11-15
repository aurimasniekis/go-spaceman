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

const stubs: Array<[object, string, () => unknown]> = [
  [Element.prototype, 'getClientRects', emptyRectList],
  [Element.prototype, 'getBoundingClientRect', emptyRect],
  [Range.prototype, 'getClientRects', emptyRectList],
  [Range.prototype, 'getBoundingClientRect', emptyRect],
]

for (const [proto, name, make] of stubs) {
  if (!(name in proto)) {
    Object.defineProperty(proto, name, { configurable: true, writable: true, value: make })
  }
}
