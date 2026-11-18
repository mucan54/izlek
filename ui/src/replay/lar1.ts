// LAR1 decoding (spec/replay.md). The decoder and the scene reducer are the
// web SDK's own standalone module, so the capture side and the player cannot
// drift apart; test/unit/lar1.test.ts checks it against spec/vectors/replay.json.
export { applyFrame, decodeChunk, newScene, ReplayDecodeError } from '../../../sdk-web/src/replay-decode'
export type { Chunk, Frame, RNode, Scene } from '../../../sdk-web/src/replay-decode'

/** Node types. */
export const CONTAINER = 0
export const TEXT = 1
export const IMAGE = 2
export const BUTTON = 3
export const INPUT = 4
export const SCROLL = 5
export const OTHER = 6

/** Node flag bits. */
export const HAS_COLOR = 1
export const HAS_TEXT = 2
export const MASKED = 4
export const CLICKABLE = 8
export const FOCUSED = 16

/** Frame kinds. */
export const FULL_SNAPSHOT = 0
export const DELTA = 1
export const TOUCH = 2
export const SCROLL_FRAME = 3
export const SCREEN_CHANGE = 4
export const VIEWPORT = 5
