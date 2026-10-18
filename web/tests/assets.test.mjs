import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { shellVersion } from '../vite.config.ts';

test('icon-only and other public asset changes invalidate the offline shell', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'mirrorbird-shell-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const bundle = ['index.html', 'assets/app.js', 'assets/app.css'];
  for (const file of ['favicon.svg', 'icon-192.png', 'icon-512.png', 'manifest.webmanifest', 'sw.js']) {
    writeFileSync(join(root, file), file);
  }
  const version = shellVersion(bundle, root);
  assert.equal(shellVersion([...bundle].reverse(), root), version);
  assert.match(version, /^[a-f0-9]{16}$/);
  for (const file of ['favicon.svg', 'icon-192.png', 'icon-512.png', 'manifest.webmanifest', 'sw.js']) {
    writeFileSync(join(root, file), `${file} changed`);
    assert.notEqual(shellVersion(bundle, root), version, `${file} must invalidate the cache`);
    writeFileSync(join(root, file), file);
    assert.equal(shellVersion(bundle, root), version);
  }
  renameSync(join(root, 'favicon.svg'), join(root, 'renamed.svg'));
  assert.notEqual(shellVersion(bundle, root), version);
  renameSync(join(root, 'renamed.svg'), join(root, 'favicon.svg'));
  mkdirSync(join(root, 'nested'));
  writeFileSync(join(root, 'nested', 'new.png'), 'new image');
  assert.notEqual(shellVersion(bundle, root), version);
});

test('generated website, PWA and native artwork use the same source assets', () => {
  for (const file of ['favicon.svg', 'icon-192.png', 'icon-512.png']) {
    assert.deepEqual(readFileSync(new URL(`../static/${file}`, import.meta.url)),
      readFileSync(new URL(`../public/${file}`, import.meta.url)));
  }
  for (const size of [192, 512]) {
    const png = readFileSync(new URL(`../public/icon-${size}.png`, import.meta.url));
    assert.equal(png.subarray(1, 4).toString(), 'PNG');
    assert.equal(png.readUInt32BE(16), size);
    assert.equal(png.readUInt32BE(20), size);
  }
  assert.deepEqual(readFileSync(new URL('../public/icon-512.png', import.meta.url)),
    readFileSync(new URL('../../internal/desktop/app-icon.png', import.meta.url)));
  const config = JSON.parse(readFileSync(new URL('../../mygo.json', import.meta.url), 'utf8'));
  assert.equal(config.icon, 'internal/desktop/app-icon.png');
});
