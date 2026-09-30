import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { resolve } from 'node:path';
const root = resolve(import.meta.dirname, '..');
const source = resolve(root, 'third_party/react-grab-toolbar');
const target = resolve(root, 'web/app-pill/src/grab');
mkdirSync(target, { recursive: true });
for (const name of ['toolbar-position.ts', 'create-toolbar-drag.ts', 'clamp-to-range.ts']) {
  let text = readFileSync(resolve(source, name), 'utf8');
  text = text.replaceAll('../types.js', './types.js').replaceAll('../components/toolbar/state.js', './types.js').replaceAll('../constants.js', './constants.js');
  text = text.replaceAll('./native-raf.js', './runtime.js').replaceAll('./runtime-mode.js', './runtime.js');
  text = text.replace('if (config.isCollapsed() || isSnapping()) return;', 'cancelSnapAnimationFrame();\n    clearTimeout(snapAnimationTimeout);\n    setIsSnapping(false);');
  text = text.replace('const currentVelocity = velocity();', 'const currentVelocity = performance.now() - lastPointerPosition.time > 100 ? { x: 0, y: 0 } : velocity();');
  text = text.replace('pointerStartPosition = { x: event.clientX, y: event.clientY };', 'config.onPositionUpdate({ x: rect.left, y: rect.top });\n    pointerStartPosition = { x: event.clientX, y: event.clientY };');
  text = text.replace('if (event.button !== 0) return;', 'if (event.button !== 0) return;\n    didDragOccur = false;');
  text = text.replaceAll('(callback: () => void)', '(callback?: () => void)');
  text = text.replace('if (didDragOccur) {', 'if (didDragOccur) {\n      event.preventDefault();');
  text = text.replace('callback();', 'callback?.();');
  writeFileSync(resolve(target, name), text);
}

for (const name of ['toolbar-content.tsx', 'icon-chevron.tsx', 'cn.ts']) {
  const text = readFileSync(resolve(source, name), 'utf8')
    .replace('../../utils/cn.js', './cn.js')
    .replace('../../utils/toolbar-position.js', './toolbar-position.js')
    .replace('../icons/icon-chevron.jsx', './icon-chevron.jsx');
  writeFileSync(resolve(target, name), text);
}
