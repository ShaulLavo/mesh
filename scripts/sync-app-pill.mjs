import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { resolve } from 'node:path';
const root = resolve(import.meta.dirname, '..');
const source = resolve(root, 'third_party/react-grab-toolbar');
const target = resolve(root, 'web/app-pill/src/grab');
mkdirSync(target, { recursive: true });
for (const name of ['toolbar-content.tsx', 'icon-chevron.tsx', 'cn.ts']) {
  const text = readFileSync(resolve(source, name), 'utf8')
    .replace('../../utils/cn.js', './cn.js')
    .replace('../../utils/toolbar-position.js', '../dock.js')
    .replace('../icons/icon-chevron.jsx', './icon-chevron.jsx');
  writeFileSync(resolve(target, name), text);
}
