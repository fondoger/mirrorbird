import { defineConfig } from 'vite';
import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

export function shellVersion(bundleFiles: string[], publicDir: string): string {
  const hash = createHash('sha256').update([...bundleFiles].sort().join('\n'));
  for (const file of readdirSync(publicDir, { recursive: true, withFileTypes: true }).filter((entry) => entry.isFile())
    .map((entry) => join(entry.parentPath, entry.name)).sort()) {
    hash.update(relative(publicDir, file).split(sep).join('/')).update('\0').update(readFileSync(file)).update('\0');
  }
  return hash.digest('hex').slice(0, 16);
}

export default defineConfig({
  base: '/',
  server: { host: '127.0.0.1', port: 5173, strictPort: true },
  preview: { host: '127.0.0.1', port: 4173, strictPort: true },
  plugins: [{
    name: 'version-offline-shell',
    writeBundle(_options, bundle) {
      const publicDir = fileURLToPath(new URL('./public/', import.meta.url));
      const version = shellVersion(Object.keys(bundle), publicDir);
      const source = readFileSync(new URL('./public/sw.js', import.meta.url), 'utf8');
      writeFileSync(fileURLToPath(new URL('./static/sw.js', import.meta.url)), source.replace('mirrorbird-shell-v1', `mirrorbird-shell-${version}`));
    },
  }],
  build: {
    outDir: 'static',
    emptyOutDir: true,
    target: 'es2022',
    sourcemap: false,
    assetsInlineLimit: 0,
  },
});
