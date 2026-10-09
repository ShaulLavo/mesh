import { createEffect, createMemo, createSignal, onCleanup, onMount, For, type JSX } from 'solid-js';
import { render } from 'solid-js/web';
import './pill.css';
import { DOT_TARGET, confine, dockFromRelease, dotLeads, isHorizontalEdge, placeDot, placePill, type Box, type Dock, type Insets, type Point } from './dock';
import { createDrag } from './drag';

const SNAP_MS = 250;
// The opening morph ends in a transitionend; this only guards against a missed event.
const REVEAL_FALLBACK_MS = 1500;
const DOT_RING_RADIUS = 11;
// The open clip extends past the pill so its shadow is not cut off.
const OPEN_CLIP = 'inset(-16px round 38px)';

function loadDock(storageKey: string): Dock {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(storageKey) ?? 'null');
    if (typeof value !== 'object' || value === null || !('edge' in value) || !('ratio' in value)) return { edge: 'bottom', ratio: .5 };
    const { edge, ratio } = value;
    if ((edge === 'top' || edge === 'bottom' || edge === 'left' || edge === 'right') && typeof ratio === 'number' && Number.isFinite(ratio)) return { edge, ratio: Math.max(0, Math.min(1, ratio)) };
  } catch { /* Browser storage may be disabled. */ }
  return { edge: 'bottom', ratio: .5 };
}

type ActionPresentation = { id: string; label: string; title?: string; content?: JSX.Element };
export type FloatingPillAction = ActionPresentation & (
  | { kind: 'button'; onSelect: () => void }
  | { kind: 'link'; href: string; target?: '_self' | '_blank' }
);
export type FloatingPillProps = {
  actions: readonly FloatingPillAction[];
  stylesheetHref: string;
  label?: string;
  storageKey?: string;
};

function Action(props: { action: FloatingPillAction; dragAware: ReturnType<typeof createDrag>['dragAware'] }) {
  const action = props.action;
  if (action.kind === 'link') return <a class="action" data-action={action.id} draggable={false} href={action.href}
    target={action.target} rel="noopener noreferrer" aria-label={action.label} title={action.title ?? action.label}
    onClick={props.dragAware()}>{action.content ?? action.label.slice(0, 1)}</a>;
  return <button class="action" data-action={action.id} type="button" aria-label={action.label}
    title={action.title ?? action.label} onClick={props.dragAware(action.onSelect)}>{action.content ?? action.label.slice(0, 1)}</button>;
}
type Layout = { dot: Point; pill: Point; pillWidth: number; pillHeight: number };

export function FloatingPill(props: FloatingPillProps) {
  const storageKey = props.storageKey ?? 'floating-pill-position';
  const [collapsed, setCollapsed] = createSignal(true);
  const [dock, setDock] = createSignal(loadDock(storageKey));
  const [layout, setLayout] = createSignal<Layout>({ dot: { x: -1000, y: -1000 }, pill: { x: -1000, y: -1000 }, pillWidth: 0, pillHeight: 0 });
  const [position, setPosition] = createSignal<Point>({ x: -1000, y: -1000 });
  const [snapping, setSnapping] = createSignal(false);
  const [resizing, setResizing] = createSignal(false);
  const [keyboardMotion, setKeyboardMotion] = createSignal(false);
  // The stylesheet loads after the elements exist; morph transitions wait for a real toggle so loading never flashes the pill.
  const [animated, setAnimated] = createSignal(false);
  // Until the opening morph lands, the actions appearing beside the dot ignore pointers, so a quick second tap stays on the dot.
  const [revealed, setRevealed] = createSignal(false);
  let shell: HTMLDivElement | undefined;
  let morph: HTMLDivElement | undefined;
  let dotButton: HTMLButtonElement | undefined;
  let safe: HTMLDivElement | undefined;
  let snapTimer: ReturnType<typeof setTimeout> | undefined;
  let revealTimer: ReturnType<typeof setTimeout> | undefined;

  const viewport = (): Box => {
    const visual = window.visualViewport;
    return { left: visual?.offsetLeft ?? 0, top: visual?.offsetTop ?? 0, width: visual?.width ?? innerWidth, height: visual?.height ?? innerHeight };
  };
  const insets = (): Insets => {
    const style = safe ? getComputedStyle(safe) : undefined;
    const inset = (name: string) => parseFloat(style?.getPropertyValue(name) ?? '0') || 0;
    return { top: inset('padding-top'), right: inset('padding-right'), bottom: inset('padding-bottom'), left: inset('padding-left') };
  };
  const box = () => collapsed() ? { width: DOT_TARGET, height: DOT_TARGET } : { width: layout().pillWidth, height: layout().pillHeight };
  const relayout = (): Layout => {
    const size = { width: morph?.offsetWidth ?? 0, height: morph?.offsetHeight ?? 0 };
    const next = { dot: placeDot(dock(), viewport(), insets()), pill: placePill(dock(), size, viewport(), insets()), pillWidth: size.width, pillHeight: size.height };
    setLayout(next);
    return next;
  };
  const redock = () => {
    if (drag.isDragging()) return;
    const next = relayout();
    setPosition(collapsed() ? next.dot : next.pill);
  };
  // Both states hang off one anchor, the dot's center, so a toggle reshapes the pill in place instead of moving it.
  const offsets = createMemo(() => {
    const { dot, pill } = layout();
    const base = collapsed() ? dot : pill;
    return { dot: { x: dot.x - base.x, y: dot.y - base.y }, pill: { x: pill.x - base.x, y: pill.y - base.y } };
  });
  const clip = () => {
    if (!collapsed()) return OPEN_CLIP;
    const { dot, pill, pillWidth, pillHeight } = layout();
    const x = dot.x + DOT_TARGET / 2 - pill.x;
    const y = dot.y + DOT_TARGET / 2 - pill.y;
    const r = DOT_RING_RADIUS;
    return `inset(${y - r}px ${pillWidth - x - r}px ${pillHeight - y - r}px ${x - r}px round ${r}px)`;
  };
  const saveDock = () => {
    try { localStorage.setItem(storageKey, JSON.stringify(dock())); } catch { /* Storage is optional. */ }
  };
  const drag = createDrag({
    element: () => shell,
    onStart: () => setKeyboardMotion(false),
    onMove: next => { clearTimeout(snapTimer); setSnapping(false); setPosition(confine(next, box(), viewport(), insets())); },
    onRelease: (released, velocity) => {
      const { dot, pill } = layout();
      const anchor = collapsed() ? { x: DOT_TARGET / 2, y: DOT_TARGET / 2 } : { x: dot.x + DOT_TARGET / 2 - pill.x, y: dot.y + DOT_TARGET / 2 - pill.y };
      setDock(dockFromRelease(released, anchor, velocity, viewport(), insets()));
      setSnapping(true);
      saveDock();
      redock();
      clearTimeout(snapTimer);
      snapTimer = setTimeout(() => setSnapping(false), SNAP_MS);
    },
  });
  const reveal = () => { clearTimeout(revealTimer); if (!collapsed()) setRevealed(true); };
  const setOpen = (open: boolean) => {
    // A toggle can interrupt a snap. Starting the new state from the rendered box keeps the dot under the finger,
    // and the snap then carries on toward the dock while the morph runs.
    const rendered = shell?.getBoundingClientRect();
    const { dot, pill } = relayout();
    const from = collapsed() ? dot : pill;
    const to = open ? pill : dot;
    const carry = rendered ? { x: rendered.left - from.x, y: rendered.top - from.y } : { x: 0, y: 0 };
    clearTimeout(snapTimer);
    clearTimeout(revealTimer);
    setSnapping(false);
    setAnimated(true);
    setRevealed(false);
    setCollapsed(!open);
    setPosition({ x: to.x + carry.x, y: to.y + carry.y });
    if (Math.hypot(carry.x, carry.y) > 0.5) {
      void shell?.offsetWidth;
      setSnapping(true);
      setPosition(to);
      snapTimer = setTimeout(() => setSnapping(false), SNAP_MS);
    }
    if (open) revealTimer = setTimeout(reveal, REVEAL_FALLBACK_MS);
  };
  createEffect(() => { collapsed(); props.actions; dock(); queueMicrotask(redock); });
  onMount(() => {
    const abort = new AbortController();
    const { signal } = abort;
    let viewportFrame = 0;
    let viewportTimer: ReturnType<typeof setTimeout> | undefined;
    const updateViewport = () => {
      if (drag.isDragging()) return;
      setResizing(true);
      clearTimeout(viewportTimer);
      viewportTimer = setTimeout(() => setResizing(false), 150);
      if (viewportFrame) return;
      viewportFrame = requestAnimationFrame(() => { viewportFrame = 0; redock(); });
    };
    // An edge change can turn the pill vertical; re-placing it mid-snap retargets the transition smoothly.
    const observer = new ResizeObserver(redock);
    if (morph) observer.observe(morph);
    window.addEventListener('resize', updateViewport, { signal, passive: true });
    window.visualViewport?.addEventListener('resize', updateViewport, { signal, passive: true });
    window.visualViewport?.addEventListener('scroll', updateViewport, { signal, passive: true });
    onCleanup(() => { abort.abort(); observer.disconnect(); clearTimeout(snapTimer); clearTimeout(revealTimer); clearTimeout(viewportTimer); cancelAnimationFrame(viewportFrame); });
    redock();
  });
  const keyboard = (event: KeyboardEvent) => {
    setKeyboardMotion(true);
    if (event.key === 'Escape') { setOpen(false); dotButton?.focus({ preventScroll: true }); return; }
    const docks: Record<string, Dock['edge']> = { ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'top', ArrowDown: 'bottom' };
    const next = docks[event.key];
    if (!next || !event.altKey) return;
    event.preventDefault(); setDock({ edge: next, ratio: dock().ratio }); redock(); saveDock();
  };
  return <>
    <link rel="stylesheet" href={props.stylesheetHref}/><div class="safe" ref={safe}/>
    <div ref={shell} class="shell" role="toolbar" aria-label={props.label ?? "Floating controls"}
      classList={{ closed: collapsed(), vertical: !isHorizontalEdge(dock().edge), dragging: drag.isDragging(), snapping: snapping(), animated: animated(), revealed: revealed(), resizing: resizing(), keyboard: keyboardMotion() }}
      style={{ transform: `translate3d(${position().x}px,${position().y}px,0)`, width: `${box().width}px`, height: `${box().height}px` }} onKeyDown={keyboard}
      onPointerDown={event => { setKeyboardMotion(false); drag.handlePointerDown(event); }}>
      <button ref={dotButton} class="dot-target" type="button" aria-label={collapsed() ? `Open ${props.label ?? 'floating controls'}. Drag to move; Alt and arrow keys to dock.` : `Close ${props.label ?? 'floating controls'}`} aria-expanded={!collapsed()}
        style={{ left: `${offsets().dot.x}px`, top: `${offsets().dot.y}px` }} onClick={drag.dragAware(() => setOpen(collapsed()))}><span class="dot"/></button>
      <div ref={morph} class="morph" inert={collapsed()} aria-hidden={collapsed()} onTransitionEnd={event => { if (event.target === morph) reveal(); }} onTransitionCancel={event => { if (event.target === morph) reveal(); }} style={{ left: `${offsets().pill.x}px`, top: `${offsets().pill.y}px`, 'clip-path': clip() }}>
        <div class="panel" classList={{ trailing: !dotLeads(dock()) }}>
          <span class="dot-slot"/>
          <div class="controls">
            <For each={props.actions}>{action => <Action action={action} dragAware={drag.dragAware}/>}</For>
          </div>
        </div>
      </div>
    </div>
  </>;

}

export function mountFloatingPill(options: FloatingPillProps & { parent?: HTMLElement }): () => void {
  const host = document.createElement('floating-pill');
  host.setAttribute('data-floating-pill-host', '');
  host.style.all = 'initial';
  host.style.position = 'fixed';
  host.style.zIndex = '2147483647';
  host.style.pointerEvents = 'none';
  (options.parent ?? document.documentElement).append(host);
  const dispose = render(() => <FloatingPill {...options}/>, host.attachShadow({ mode: 'open' }));
  return () => { dispose(); host.remove(); };
}
