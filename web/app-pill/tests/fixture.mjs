export function fixturePage({ mount = true } = {}) {
  const initializer = mount ? `
    document.addEventListener('DOMContentLoaded', () => {
      window.actionCalls = 0;
      window.disposePill = FloatingPill.mountFloatingPill({
        stylesheetHref: '/pill.css',
        actions: [
          { id: 'run', kind: 'button', label: 'Run action', content: '●', onSelect: () => window.actionCalls++ },
          { id: 'details', kind: 'link', label: 'Open details', content: '↗', href: '/details' },
        ],
      });
    }, { once: true });` : '';
  return `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><script defer nonce="test" src="/pill.js"></script><script nonce="test">${initializer}</script></head><body><h1>Floating pill fixture</h1><p>Actions belong to the caller.</p></body></html>`;
}
