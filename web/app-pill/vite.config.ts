import { defineConfig } from 'vite';
import { readFileSync } from 'node:fs';
import solid from 'vite-plugin-solid';
import tailwindcss from '@tailwindcss/vite';

const notices = ['solid-js', 'react-grab-toolbar'].map(name =>
  readFileSync(new URL(`../../third_party/${name}/LICENSE`, import.meta.url), 'utf8'),
).join('\n');

export default defineConfig({
  plugins: [
    solid(),
    tailwindcss(),
    {
      name: 'mesh-pill-notices',
      generateBundle(_, bundle) {
        const script = Object.values(bundle).find(asset => asset.type === 'chunk');
        if (script?.type === 'chunk') script.code = `/*!\n${notices}*/\n${script.code}`;
      },
    },
  ],
  build: {
    target: 'es2022',
    outDir: '../../internal/apppill/assets',
    emptyOutDir: true,
    lib: {
      entry: 'src/index.tsx',
      name: 'MeshAppPill',
      cssFileName: 'pill',
      formats: ['iife'],
      fileName: () => 'pill.js',
    },
  },
});
