import { defineConfig } from 'vite';
import { readFileSync } from 'node:fs';
import solid from 'vite-plugin-solid';

const notices = ['solid-js', 'react-grab-toolbar'].map(name =>
  readFileSync(new URL(`../../third_party/${name}/LICENSE`, import.meta.url), 'utf8'),
).join('\n');

export default defineConfig({
  plugins: [
    solid(),
    {
      name: 'mesh-pill-styles',
      generateBundle(_, bundle) {
        const script = Object.values(bundle).find(asset => asset.type === 'chunk');
        if (script?.type === 'chunk') script.code = `/*!\n${notices}*/\n${script.code}`;
        this.emitFile({
          type: 'asset',
          fileName: 'pill.css',
          source: readFileSync(new URL('./src/pill.css', import.meta.url), 'utf8'),
        });
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
      formats: ['iife'],
      fileName: () => 'pill.js',
    },
  },
});
