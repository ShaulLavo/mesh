import { createEffect, createSignal, onCleanup, onMount, Show } from 'solid-js';
import { render } from 'solid-js/web';
import { createToolbarDrag } from './grab/create-toolbar-drag';
import { getPositionFromEdgeAndRatio } from './grab/toolbar-position';
import type { Position, SnapEdge } from './grab/types';


function loadDock(): { edge: SnapEdge; ratio: number } {
  try {
    const value: unknown = JSON.parse(localStorage.getItem('mesh-app-pill-position') ?? 'null');
    if (typeof value !== 'object' || value === null || !('edge' in value) || !('ratio' in value)) return { edge: 'right', ratio: .8 };
    const { edge, ratio } = value;
    if ((edge === 'top' || edge === 'bottom' || edge === 'left' || edge === 'right') && typeof ratio === 'number' && Number.isFinite(ratio)) return { edge, ratio: Math.max(0, Math.min(1, ratio)) };
  } catch { /* Browser storage may be disabled. */ }
  return { edge: 'right', ratio: .8 };
}

function Pill(props: { appID: string; manager: string; nonce: string }) {
  const dock = loadDock();
  const [collapsed, setCollapsed] = createSignal(true);
  const [menu, setMenu] = createSignal(false);
  const [edge, setEdge] = createSignal(dock.edge);
  const [ratio, setRatio] = createSignal(dock.ratio);
  const [position, setPosition] = createSignal<Position>({ x: -1000, y: -1000 });
  const [copied, setCopied] = createSignal(false);
  const [keyboardMotion, setKeyboardMotion] = createSignal(false);
  const [status, setStatus] = createSignal('App');
  let shell: HTMLDivElement | undefined;
  let frame: HTMLIFrameElement | undefined;
  let handle: HTMLButtonElement | undefined;
  let safe: HTMLDivElement | undefined;
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  const dimensions = () => ({ width: shell?.offsetWidth ?? 44, height: shell?.offsetHeight ?? 44 });
  const constrain = (next: Position): Position => {
    const viewport = window.visualViewport;
    const width = viewport?.width ?? window.innerWidth;
    const height = viewport?.height ?? window.innerHeight;
    const left = viewport?.offsetLeft ?? 0;
    const top = viewport?.offsetTop ?? 0;
    const insets = safe ? getComputedStyle(safe) : undefined;
    const inset = (name: string) => parseFloat(insets?.getPropertyValue(name) ?? '0') || 0;
    const minX = left + Math.max(8, inset('padding-left'));
    const minY = top + Math.max(8, inset('padding-top'));
    const size = dimensions();
    return { x: Math.max(minX, Math.min(next.x, left + width - size.width - Math.max(8, inset('padding-right')))), y: Math.max(minY, Math.min(next.y, top + height - size.height - Math.max(8, inset('padding-bottom')))) };
  };
  const redock = () => {
    const size = dimensions();
    setPosition(constrain(getPositionFromEdgeAndRatio(edge(), ratio(), size.width, size.height)));
  };
  const saveDock = () => {
    try { localStorage.setItem('mesh-app-pill-position', JSON.stringify({ edge: edge(), ratio: ratio() })); } catch { /* Storage is optional. */ }
  };
  const drag = createToolbarDrag({
    getContainerRef: () => shell, isCollapsed: collapsed, getExpandedDimensions: dimensions,
    onDragStart: () => setMenu(false), onPositionUpdate: next => setPosition(constrain(next)),
    onSnapEdgeChange: (nextEdge, nextRatio) => { setEdge(nextEdge); setRatio(nextRatio); },
    onSnapComplete: result => { setPosition(constrain(result.position)); saveDock(); },
  });
  const toggle = drag.createDragAwareHandler(() => { setCollapsed(value => !value); setMenu(false); });
  const copy = async () => {
    try { await navigator.clipboard.writeText(location.origin); setCopied(true); clearTimeout(copyTimer); copyTimer = setTimeout(() => setCopied(false), 1800); }
    catch { window.prompt('Copy app link', location.origin); }
  };
  createEffect(() => { collapsed(); menu(); edge(); queueMicrotask(redock); });
  onMount(() => {
    const abort = new AbortController();
    const { signal } = abort;
    const observer = new ResizeObserver(redock);
    if (shell) observer.observe(shell);
    window.addEventListener('resize', redock, { signal });
    window.visualViewport?.addEventListener('resize', redock, { signal });
    window.visualViewport?.addEventListener('scroll', redock, { signal });
    window.addEventListener('message', event => {
      if (event.origin !== props.manager || event.source !== frame?.contentWindow) return;
      const value: unknown = event.data;
      if (typeof value !== 'object' || value === null || !('type' in value) || value.type !== 'mesh-app-status' || !('visibility' in value)) return;
      if (value.visibility === 'public' || value.visibility === 'private') setStatus(value.visibility === 'public' ? 'Public' : 'Private');
    }, { signal });
    onCleanup(() => { abort.abort(); observer.disconnect(); clearTimeout(copyTimer); });
    redock();
  });
  const keyboard = (event: KeyboardEvent) => {
    setKeyboardMotion(true);
    if (event.key === 'Escape') { setMenu(false); setCollapsed(true); handle?.focus(); return; }
    const docks: Record<string, SnapEdge> = { ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'top', ArrowDown: 'bottom' };
    const next = docks[event.key];
    if (!next || !event.altKey) return;
    event.preventDefault(); setEdge(next); redock(); saveDock();
  };
  return <>
    <link rel="stylesheet" href="/.mesh-app/pill.css"/><div class="safe" ref={safe}/>
    <div ref={shell} class="shell" classList={{ closed: collapsed(), dragging: drag.isDragging(), keyboard: keyboardMotion() }} style={{ transform: `translate3d(${position().x}px,${position().y}px,0)` }} onKeyDown={keyboard}>
      <div class="surface" role="toolbar" aria-label="Mesh app controls">
        <button ref={handle} class="handle" aria-label={collapsed() ? 'Open Mesh controls. Drag to move; Alt and arrow keys to dock.' : 'Collapse Mesh controls'} aria-expanded={!collapsed()} onPointerDown={event => { setKeyboardMotion(false); drag.handlePointerDown(event); }} onClick={toggle}><span class="dot"/></button>
        <Show when={!collapsed()}><div class="controls"><span class="status">{status()}</span><button class="action" onClick={() => void copy()}>{copied() ? 'Copied' : 'Copy link'}</button><button class="action" aria-label="Owner controls" aria-expanded={menu()} onClick={() => setMenu(value => !value)}>...</button></div></Show>
      </div>
      <Show when={!collapsed() && menu()}><iframe ref={frame} class="frame" title="Mesh owner controls" src={`${props.manager}/frame?id=${encodeURIComponent(props.appID)}`} referrerPolicy="no-referrer"/></Show>
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
