// Adapted from React Grab's create-toolbar-drag (MIT, third_party/react-grab-toolbar).
// Snapping moved to dock.ts so every placement path shares one rule.
import { createSignal, onCleanup, type Accessor } from 'solid-js';
import type { Point } from './dock';

const DRAG_THRESHOLD_PX = 5;
// A pointer that rested before release is a drop, not a flick.
const STALE_VELOCITY_MS = 100;

interface DragConfig {
  element: () => HTMLElement | undefined;
  onStart: () => void;
  onMove: (position: Point) => void;
  onRelease: (box: DOMRect, velocity: Point) => void;
}

export interface Drag {
  isDragging: Accessor<boolean>;
  handlePointerDown: (event: PointerEvent) => void;
  dragAware: (callback?: () => void) => (event: MouseEvent) => void;
}

export function createDrag(config: DragConfig): Drag {
  const [isDragging, setIsDragging] = createSignal(false);
  let moved = false;
  let suppressClick = false;
  let offset: Point = { x: 0, y: 0 };
  let start: Point = { x: 0, y: 0 };
  let last = { x: 0, y: 0, time: 0 };
  let velocity: Point = { x: 0, y: 0 };
  let listeners: AbortController | undefined;
  const stop = () => { listeners?.abort(); listeners = undefined; };

  const move = (event: PointerEvent) => {
    if (!moved) {
      if (Math.hypot(event.clientX - start.x, event.clientY - start.y) <= DRAG_THRESHOLD_PX) return;
      moved = true;
      config.onStart();
    }
    const now = performance.now();
    const elapsed = now - last.time;
    if (elapsed > 0) velocity = { x: (event.clientX - last.x) / elapsed, y: (event.clientY - last.y) / elapsed };
    last = { x: event.clientX, y: event.clientY, time: now };
    config.onMove({ x: event.clientX - offset.x, y: event.clientY - offset.y });
  };

  const release = () => {
    stop();
    setIsDragging(false);
    if (!moved) return;
    suppressClick = true;
    const box = config.element()?.getBoundingClientRect();
    if (box) config.onRelease(box, performance.now() - last.time > STALE_VELOCITY_MS ? { x: 0, y: 0 } : velocity);
  };

  const handlePointerDown = (event: PointerEvent) => {
    if (event.button !== 0) return;
    suppressClick = false;
    // Read the on-screen box before onMove cancels any snap in flight, so a grab starts where the eye sees it.
    const box = config.element()?.getBoundingClientRect();
    if (!box) return;
    config.onMove({ x: box.left, y: box.top });
    start = { x: event.clientX, y: event.clientY };
    offset = { x: event.clientX - box.left, y: event.clientY - box.top };
    last = { ...start, time: performance.now() };
    velocity = { x: 0, y: 0 };
    moved = false;
    setIsDragging(true);
    stop();
    listeners = new AbortController();
    const { signal } = listeners;
    window.addEventListener('pointermove', move, { signal });
    window.addEventListener('pointerup', release, { signal });
    window.addEventListener('pointercancel', release, { signal });
  };

  // A drag ends in a click on whatever was under the pointer; swallow it once.
  const dragAware = (callback?: () => void) => (event: MouseEvent) => {
    event.stopImmediatePropagation();
    if (suppressClick) {
      event.preventDefault();
      suppressClick = false;
      return;
    }
    callback?.();
  };

  onCleanup(stop);
  return { isDragging, handlePointerDown, dragAware };
}
