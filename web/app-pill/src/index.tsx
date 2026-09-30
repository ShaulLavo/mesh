import { createEffect, createSignal, onCleanup, onMount, Show } from 'solid-js';
import { render } from 'solid-js/web';
import { createToolbarDrag } from './grab/create-toolbar-drag';
import './pill.css';
import { ToolbarContent } from './grab/toolbar-content';
import { getPositionFromEdgeAndRatio, isHorizontalEdge } from './grab/toolbar-position';
import type { Position, SnapEdge } from './grab/types';
function loadDock(): { edge: SnapEdge; ratio: number } {
  try {
    const value: unknown = JSON.parse(localStorage.getItem('mesh-app-pill-position') ?? 'null');
    if (typeof value !== 'object' || value === null || !('edge' in value) || !('ratio' in value)) return { edge: 'bottom', ratio: .5 };
    const { edge, ratio } = value;
    if ((edge === 'top' || edge === 'bottom' || edge === 'left' || edge === 'right') && typeof ratio === 'number' && Number.isFinite(ratio)) return { edge, ratio: Math.max(0, Math.min(1, ratio)) };
  } catch { /* Browser storage may be disabled. */ }
  return { edge: 'bottom', ratio: .5 };
}

function Pill(props: { appID: string; manager: string; nonce: string }) {
  const dock = loadDock();
  const [collapsed, setCollapsed] = createSignal(true);
  const [edge, setEdge] = createSignal(dock.edge);
  const [ratio, setRatio] = createSignal(dock.ratio);
  const [position, setPosition] = createSignal<Position>({ x: -1000, y: -1000 });
  const [copyState, setCopyState] = createSignal<'idle' | 'copied' | 'failed'>('idle');
  const [resizing, setResizing] = createSignal(false);
  const [pressed, setPressed] = createSignal(false);
  const [keyboardMotion, setKeyboardMotion] = createSignal(false);
  const [access, setAccess] = createSignal<{ visibility: 'public' | 'private'; owns: boolean }>();
  let shell: HTMLDivElement | undefined;
  let frame: HTMLIFrameElement | undefined;
  const focusHandle = () => shell?.querySelector<HTMLButtonElement>('[data-react-grab-toolbar-collapse]')?.focus();
  let safe: HTMLDivElement | undefined;
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  const dimensions = () => ({ width: shell?.offsetWidth ?? 30, height: shell?.offsetHeight ?? 16 });
  const constrain = (next: Position): Position => {
    const viewport = window.visualViewport;
    const width = viewport?.width ?? window.innerWidth;
    const height = viewport?.height ?? window.innerHeight;
    const left = viewport?.offsetLeft ?? 0;
    const top = viewport?.offsetTop ?? 0;
    const insets = safe ? getComputedStyle(safe) : undefined;
    const inset = (name: string) => parseFloat(insets?.getPropertyValue(name) ?? '0') || 0;
    const margin = collapsed() ? 0 : 16;
    const minX = left + Math.max(margin, inset('padding-left'));
    const minY = top + Math.max(margin, inset('padding-top'));
    const size = dimensions();
    return { x: Math.max(minX, Math.min(next.x, left + width - size.width - Math.max(margin, inset('padding-right')))), y: Math.max(minY, Math.min(next.y, top + height - size.height - Math.max(margin, inset('padding-bottom')))) };
  };
  const redock = () => {
    if (drag.isDragging()) return;
    const size = dimensions();
    const next = getPositionFromEdgeAndRatio(edge(), ratio(), size.width, size.height);
    const viewport = window.visualViewport;
    if (collapsed() && edge() === 'left') next.x = viewport?.offsetLeft ?? 0;
    if (collapsed() && edge() === 'right') next.x = (viewport?.offsetLeft ?? 0) + (viewport?.width ?? innerWidth) - size.width;
    if (collapsed() && edge() === 'top') next.y = viewport?.offsetTop ?? 0;
    if (collapsed() && edge() === 'bottom') next.y = (viewport?.offsetTop ?? 0) + (viewport?.height ?? innerHeight) - size.height;
    setPosition(constrain(next));
  };
  const saveDock = () => {
    try { localStorage.setItem('mesh-app-pill-position', JSON.stringify({ edge: edge(), ratio: ratio() })); } catch { /* Storage is optional. */ }
  };
  const drag = createToolbarDrag({
    getContainerRef: () => shell, isCollapsed: collapsed, getExpandedDimensions: dimensions,
    onDragStart: () => setKeyboardMotion(false), onPositionUpdate: next => setPosition(constrain(next)),
    onSnapEdgeChange: (nextEdge, nextRatio) => { setEdge(nextEdge); setRatio(nextRatio); },
    onSnapComplete: result => { setPosition(constrain(result.position)); saveDock(); },
  });
  const toggle = drag.createDragAwareHandler(() => setCollapsed(value => !value));
  const copy = async () => {
    clearTimeout(copyTimer);
    try { await navigator.clipboard.writeText(location.origin); setCopyState('copied'); }
    catch { setCopyState('failed'); }
    copyTimer = setTimeout(() => setCopyState('idle'), 1800);
  };
  createEffect(() => { collapsed(); access(); edge(); queueMicrotask(redock); });
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
    const observer = new ResizeObserver(() => { if (!drag.isDragging() && !drag.isSnapping()) redock(); });
    if (shell) observer.observe(shell);
    window.addEventListener('resize', updateViewport, { signal, passive: true });
    window.visualViewport?.addEventListener('resize', updateViewport, { signal, passive: true });
    window.visualViewport?.addEventListener('scroll', updateViewport, { signal, passive: true });
    const refreshAccess = () => {
      if (!frame) return;
      frame.src = `${props.manager}/frame?id=${encodeURIComponent(props.appID)}`;
    };
    window.addEventListener('focus', refreshAccess, { signal });
    window.addEventListener('pageshow', refreshAccess, { signal });
    document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshAccess(); }, { signal });
    window.addEventListener('message', event => {
      if (event.origin !== props.manager || event.source !== frame?.contentWindow) return;
      const value: unknown = event.data;
      if (typeof value !== 'object' || value === null || !('type' in value) || value.type !== 'mesh-app-status' || !('visibility' in value)) return;
      if (!('owns' in value) || typeof value.owns !== 'boolean') return;
      if (value.visibility === 'public' || value.visibility === 'private') setAccess({ visibility: value.visibility, owns: value.owns });
    }, { signal });
    onCleanup(() => { abort.abort(); observer.disconnect(); clearTimeout(copyTimer); clearTimeout(viewportTimer); cancelAnimationFrame(viewportFrame); });
    redock();
  });
  const keyboard = (event: KeyboardEvent) => {
    setKeyboardMotion(true);
    if (event.key === 'Escape') { setCollapsed(true); focusHandle(); return; }
    const docks: Record<string, SnapEdge> = { ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'top', ArrowDown: 'bottom' };
    const next = docks[event.key];
    if (!next || !event.altKey) return;
    event.preventDefault(); setEdge(next); redock(); saveDock();
  };
  return <>
    <link rel="stylesheet" href="/.mesh-app/pill.css"/><div class="safe" ref={safe}/>
    <div ref={shell} class="shell" role="toolbar" aria-label="Mesh app controls"
      classList={{ closed: collapsed(), vertical: !isHorizontalEdge(edge()), dragging: drag.isDragging(), snapping: drag.isSnapping(), resizing: resizing(), keyboard: keyboardMotion() }}
      style={{ transform: `translate3d(${position().x}px,${position().y}px,0)` }} onKeyDown={keyboard}
      onPointerDown={event => { setKeyboardMotion(false); drag.handlePointerDown(event); }}>
      <ToolbarContent isCollapsed={collapsed()} snapEdge={edge()} isChevronPressed={pressed()}
        onCollapseClick={toggle} onCollapsePointerDown={() => setPressed(true)} onCollapsePointerUp={() => setPressed(false)} onCollapsePointerLeave={() => setPressed(false)}
        actionButtons={<div class="controls" classList={{ 'flex-col': !isHorizontalEdge(edge()) }} style={{ display: 'flex' }} inert={collapsed()} aria-hidden={collapsed()}>
          <div class="action-wrap">
            <button class="action" classList={{ copied: copyState() === 'copied' }} aria-label={copyState() === 'copied' ? 'Link copied' : 'Copy link'} title={copyState() === 'copied' ? 'Copied' : 'Copy link'} onClick={drag.createDragAwareHandler(() => void copy())}>
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <Show when={copyState() === 'copied'} fallback={<><path d="M10 13a5 5 0 0 0 7 .2l3-3a5 5 0 0 0-7-7l-2 2"/><path d="M14 11a5 5 0 0 0-7-.2l-3 3a5 5 0 0 0 7 7l2-2"/></>}><path d="m5 12 4 4L19 6"/></Show>
              </svg>
            </button>
          </div>
          <Show when={access()}>{current => {
            const action = () => current().visibility === 'public' ? 'private' : 'public';
            const label = () => current().owns ? `Make ${action()}` : 'Pair owner browser';
            return <div class="action-wrap"><a class="action" draggable={false} href={current().owns ? `${props.manager}/confirm?id=${encodeURIComponent(props.appID)}&action=${action()}` : `${props.manager}/pair`} target="_blank" rel="noopener noreferrer" aria-label={label()} title={label()} onClick={drag.createDragAwareHandler()}>
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><rect x="5" y="10" width="14" height="11" rx="3"/><path d={current().visibility === 'private' ? 'M8 10V7a4 4 0 0 1 8 0v3' : 'M8 10V7a4 4 0 0 1 8 0'}/><path d="M12 14v3"/></svg>
            </a></div>;
          }}</Show>
        </div>}/>
      <span class="sr-only" role="status">{copyState() === 'copied' ? 'Link copied' : ''}</span>
      <Show when={copyState() === 'failed'}><span class="copy-error" role="status">Couldn’t copy. Tap to try again.</span></Show>
      <Show when={!collapsed()}><iframe ref={frame} class="auth-frame" hidden aria-hidden="true" tabIndex={-1} title="Mesh browser authorization" src={`${props.manager}/frame?id=${encodeURIComponent(props.appID)}`} referrerPolicy="no-referrer"/></Show>
    </div>
  </>;

}

function mount() {
  const script = document.querySelector('script[data-mesh-app][data-mesh-manager]');
  if (!(script instanceof HTMLScriptElement) || document.querySelector('[data-mesh-pill-host]')) return;
  const appID = script.dataset.meshApp;
  const value = script.dataset.meshManager;
  if (!appID || !value) return;
  const config = { appID, nonce: script.nonce ?? '' };
  const manager = new URL(value);
  if (manager.protocol !== 'https:' && !(manager.protocol === 'http:' && manager.hostname === 'localhost')) return;
  const host = document.createElement('mesh-app-pill');
  host.setAttribute('data-mesh-pill-host', '');
  host.style.all = 'initial';
  host.style.position = 'fixed';
  host.style.zIndex = '2147483647';
  host.style.pointerEvents = 'none';
  document.documentElement.append(host);
  render(() => <Pill appID={config.appID} manager={manager.origin} nonce={config.nonce}/>, host.attachShadow({ mode: 'open' }));
}
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', mount, { once: true });
else mount();
