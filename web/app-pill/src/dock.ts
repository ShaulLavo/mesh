export type Edge = 'top' | 'bottom' | 'left' | 'right';
export interface Point { x: number; y: number }
export interface Size { width: number; height: number }
export interface Box { left: number; top: number; width: number; height: number }
export interface Insets { top: number; right: number; bottom: number; left: number }
// ratio places the dot's center along the edge: 0 is the start (left or top), 1 the end.
export interface Dock { edge: Edge; ratio: number }

export const GAP = 16;
export const PILL_THICKNESS = 26;
export const DOT_TARGET = 44;
// The dot rides the expanded pill's centerline, so expanding grows the pill around it instead of moving it.
export const DOT_LINE = GAP + PILL_THICKNESS / 2;
const FLICK_PROJECTION_MS = 150;

export const isHorizontalEdge = (edge: Edge): boolean => edge === 'top' || edge === 'bottom';

const clamp = (value: number, min: number, max: number) => Math.max(min, Math.min(value, max));

// Every placement measures from the visual viewport shrunk by the safe-area insets.
// A second frame anywhere is how the pill used to land flush on one path and padded on another.
const safeFrame = (viewport: Box, insets: Insets) => ({
  left: viewport.left + insets.left,
  top: viewport.top + insets.top,
  right: viewport.left + viewport.width - insets.right,
  bottom: viewport.top + viewport.height - insets.bottom,
});

export function dotCenter(dock: Dock, viewport: Box, insets: Insets): Point {
  const frame = safeFrame(viewport, insets);
  const along = (start: number, end: number) => start + DOT_LINE + Math.max(0, end - start - 2 * DOT_LINE) * clamp(dock.ratio, 0, 1);
  switch (dock.edge) {
    case 'top': return { x: along(frame.left, frame.right), y: frame.top + DOT_LINE };
    case 'bottom': return { x: along(frame.left, frame.right), y: frame.bottom - DOT_LINE };
    case 'left': return { x: frame.left + DOT_LINE, y: along(frame.top, frame.bottom) };
    case 'right': return { x: frame.right - DOT_LINE, y: along(frame.top, frame.bottom) };
  }
}

export function placeDot(dock: Dock, viewport: Box, insets: Insets): Point {
  const center = dotCenter(dock, viewport, insets);
  return { x: center.x - DOT_TARGET / 2, y: center.y - DOT_TARGET / 2 };
}

export function placePill(dock: Dock, size: Size, viewport: Box, insets: Insets): Point {
  const frame = safeFrame(viewport, insets);
  const center = dotCenter(dock, viewport, insets);
  const x = clamp(center.x - size.width / 2, frame.left + GAP, frame.right - GAP - size.width);
  const y = clamp(center.y - size.height / 2, frame.top + GAP, frame.bottom - GAP - size.height);
  switch (dock.edge) {
    case 'top': return { x, y: frame.top + GAP };
    case 'bottom': return { x, y: frame.bottom - GAP - size.height };
    case 'left': return { x: frame.left + GAP, y };
    case 'right': return { x: frame.right - GAP - size.width, y };
  }
}

// Keeps a dragged box on screen; the dock rule takes over on release.
export function confine(point: Point, size: Size, viewport: Box, insets: Insets): Point {
  const frame = safeFrame(viewport, insets);
  return { x: clamp(point.x, frame.left, frame.right - size.width), y: clamp(point.y, frame.top, frame.bottom - size.height) };
}

// anchor is the dot's center inside the released box. A pill clamped into a corner is not
// centered on its dot, so the box center alone would move the dot on a drag that went nowhere.
// velocity is in px/ms. A flick throws the release point forward before the nearest edge is chosen.
export function dockFromRelease(box: Box, anchor: Point, velocity: Point, viewport: Box, insets: Insets): Dock {
  const frame = safeFrame(viewport, insets);
  const throwX = velocity.x * FLICK_PROJECTION_MS;
  const throwY = velocity.y * FLICK_PROJECTION_MS;
  const x = box.left + box.width / 2 + throwX;
  const y = box.top + box.height / 2 + throwY;
  const distances: [Edge, number][] = [['top', y - frame.top], ['bottom', frame.bottom - y], ['left', x - frame.left], ['right', frame.right - x]];
  const [edge] = distances.reduce((nearest, next) => next[1] < nearest[1] ? next : nearest);
  const [start, end, value] = isHorizontalEdge(edge) ? [frame.left, frame.right, box.left + anchor.x + throwX] : [frame.top, frame.bottom, box.top + anchor.y + throwY];
  const span = end - start - 2 * DOT_LINE;
  return { edge, ratio: span > 0 ? clamp((value - start - DOT_LINE) / span, 0, 1) : 0.5 };
}
