import assert from 'node:assert/strict';
import { test } from 'node:test';
import { DOT_CAP, DOT_LINE, DOT_TARGET, GAP, PILL_THICKNESS, confine, dockFromRelease, dotCenter, placeDot, placePill } from '../src/dock.ts';

const edges = ['top', 'bottom', 'left', 'right'];
const none = { top: 0, right: 0, bottom: 0, left: 0 };
const portrait = { left: 0, top: 0, width: 390, height: 844 };
const landscape = { left: 0, top: 0, width: 844, height: 390 };
const notch = { top: 0, right: 47, bottom: 21, left: 47 };
const middle = { x: DOT_TARGET / 2, y: DOT_TARGET / 2 };
const pillSize = edge => edge === 'top' || edge === 'bottom' ? { width: 3 * DOT_TARGET, height: PILL_THICKNESS } : { width: PILL_THICKNESS, height: 3 * DOT_TARGET };

const gapTo = (edge, box, viewport, insets) => ({
  top: box.top - viewport.top - insets.top,
  bottom: viewport.top + viewport.height - insets.bottom - (box.top + box.height),
  left: box.left - viewport.left - insets.left,
  right: viewport.left + viewport.width - insets.right - (box.left + box.width),
})[edge];

const scenarios = [
  ['iPhone portrait', portrait, none],
  ['iPhone landscape with notch insets', landscape, notch],
  ['Safari toolbar shown and pinch offset', { left: 30, top: 120, width: 360, height: 644 }, { top: 0, right: 0, bottom: 34, left: 0 }],
];

for (const [name, viewport, insets] of scenarios) {
  test(`${name}: the dot and the pill keep one gap on every edge`, () => {
    for (const edge of edges) {
      for (const ratio of [0, 0.37, 1]) {
        const dot = placeDot({ edge, ratio }, viewport, insets);
        const center = dotCenter({ edge, ratio }, viewport, insets);
        assert.deepEqual(dot, { x: center.x - DOT_TARGET / 2, y: center.y - DOT_TARGET / 2 });
        assert.equal(gapTo(edge, { left: center.x, top: center.y, width: 0, height: 0 }, viewport, insets), DOT_LINE, `${edge} ${ratio}: dot center sits on the dock line`);
        const size = pillSize(edge);
        const pill = placePill({ edge, ratio }, size, viewport, insets);
        assert.equal(gapTo(edge, { left: pill.x, top: pill.y, ...size }, viewport, insets), GAP, `${edge} ${ratio}: pill keeps the gap`);
        for (const side of edges) assert(gapTo(side, { left: pill.x, top: pill.y, ...size }, viewport, insets) >= GAP, `${edge} ${ratio}: pill clears ${side}`);
      }
    }
  });

  test(`${name}: releasing at the rest position never moves the dot or the pill`, () => {
    for (const edge of edges) {
      for (const ratio of [0, 0.25, 0.5, 1]) {
        const dock = { edge, ratio };
        const dot = placeDot(dock, viewport, insets);
        const fromDot = dockFromRelease({ left: dot.x, top: dot.y, width: DOT_TARGET, height: DOT_TARGET }, middle, { x: 0, y: 0 }, viewport, insets);
        assert.deepEqual(placeDot(fromDot, viewport, insets), dot, `${edge} ${ratio}: dot release path matches the redock path (a corner may report either edge)`);
        const size = pillSize(edge);
        const pill = placePill(dock, size, viewport, insets);
        const anchor = { x: dot.x + DOT_TARGET / 2 - pill.x, y: dot.y + DOT_TARGET / 2 - pill.y };
        const fromPill = dockFromRelease({ left: pill.x, top: pill.y, ...size }, anchor, { x: 0, y: 0 }, viewport, insets);
        assert.equal(fromPill.edge, edge);
        assert.deepEqual(placePill(fromPill, size, viewport, insets), pill, `${edge} ${ratio}: pill release path matches the redock path`);
        assert.deepEqual(placeDot(fromPill, viewport, insets), dot, `${edge} ${ratio}: releasing the open pill where it rests keeps the dot, even when a corner clamps the pill off-center`);
      }
    }
  });
}

for (const [name, viewport, insets] of scenarios) {
  test(`${name}: the open pill holds the dot in its end cap, actions toward the middle`, () => {
    for (const edge of edges) {
      for (const ratio of [0, 0.2, 0.5, 0.51, 0.8, 1]) {
        const size = pillSize(edge);
        const pill = placePill({ edge, ratio }, size, viewport, insets);
        const center = dotCenter({ edge, ratio }, viewport, insets);
        const horizontal = edge === 'top' || edge === 'bottom';
        const along = horizontal ? center.x - pill.x : center.y - pill.y;
        const across = horizontal ? center.y - pill.y : center.x - pill.x;
        const length = horizontal ? size.width : size.height;
        assert.equal(across, DOT_CAP, `${edge} ${ratio}: the dot is on the pill's centerline`);
        assert.equal(along, ratio <= 0.5 ? DOT_CAP : length - DOT_CAP, `${edge} ${ratio}: the dot is in the cap nearer the corner`);
      }
    }
  });
}

test('a corner dot is equidistant from both edges', () => {
  assert.deepEqual(dotCenter({ edge: 'bottom', ratio: 0 }, portrait, none), { x: DOT_LINE, y: 844 - DOT_LINE });
});

test('a flick lands where it was thrown, a slow release where it was dropped', () => {
  const box = { left: 173, top: 400, width: DOT_TARGET, height: DOT_TARGET };
  assert.equal(dockFromRelease(box, middle, { x: 0, y: 0 }, portrait, none).edge, 'left', 'ties resolve to the first nearest edge');
  assert.equal(dockFromRelease(box, middle, { x: 1.5, y: 0 }, portrait, none).edge, 'right');
  assert.equal(dockFromRelease(box, middle, { x: 0, y: 3 }, portrait, none).edge, 'bottom');
  const thrown = dockFromRelease({ ...box, left: 300 }, middle, { x: 0.2, y: -0.8 }, portrait, none);
  assert.equal(thrown.edge, 'right');
  assert.equal(dotCenter(thrown, portrait, none).y, 400 + DOT_TARGET / 2 - 0.8 * 150);
  assert.equal(dockFromRelease({ left: 0, top: -400, width: DOT_TARGET, height: DOT_TARGET }, middle, { x: 0, y: 0 }, portrait, none).ratio, 0);
});

test('dragging keeps the box inside the safe frame', () => {
  assert.deepEqual(confine({ x: -50, y: 900 }, { width: DOT_TARGET, height: DOT_TARGET }, landscape, notch), { x: 47, y: 390 - 21 - DOT_TARGET });
});
