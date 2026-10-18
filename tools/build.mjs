import { spawnSync } from 'node:child_process';
import { mkdirSync, rmSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  assertMatchingSources, fileEvidence, gitEvidence, snapshotSources, sourceScope,
  toolchainEvidence, writeBuildManifest,
} from './build-evidence.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const go = process.env.MIRRORBIRD_GO || 'go';
const commands = [];

function run(command, args, cwd = root) {
  commands.push({ command: command === go ? 'go' : command, args: [...args], cwd: path.relative(root, cwd).split(path.sep).join('/') || '.' });
  const result = spawnSync(command, args, { cwd, stdio: 'inherit', env: process.env });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
}

// npm's Windows shim is a .cmd file. Use cmd only for this fixed command, never
// for user-provided paths, arguments, or an executable selected by a browser.
function npm(args) {
  if (process.platform === 'win32') {
    run('cmd.exe', ['/d', '/s', '/c', `npm ${args.join(' ')}`], path.join(root, 'web'));
  } else {
    run('npm', args, path.join(root, 'web'));
  }
}

if (process.argv.slice(2).some(arg => arg !== '--check')) {
  console.error('Usage: node tools/build.mjs [--check]');
  process.exit(2);
}

// A failed/partial build must not leave a previous success manifest beside new outputs.
const manifestPath = path.join(root, 'build', 'build-manifest.json');
rmSync(manifestPath, { force: true });
rmSync(manifestPath + '.tmp', { force: true });
const startedAt = new Date().toISOString();
const toolchain = toolchainEvidence(root, go);

npm(['ci', '--ignore-scripts', '--no-audit', '--no-fund']);
if (process.argv.includes('--check')) {
  npm(['run', 'typecheck']);
  npm(['test']);
}
npm(['run', 'build']);
// Vite intentionally rewrites web/static; freeze repository inputs only after it finishes.
const sources = snapshotSources(root);
const git = gitEvidence(root);
if (process.argv.includes('--check')) {
  run(go, ['test', './...']);
  run(go, ['vet', './...']);
}
mkdirSync(path.join(root, 'build'), { recursive: true });
const suffix = process.platform === 'win32' ? '.exe' : '';
const names = ['mirrorbird-codex', 'mirrorbird-diagnose', 'mirrorbird'];
for (const name of names) {
  const args = ['build', '-trimpath'];
  if (name === 'mirrorbird' && process.platform === 'win32') {
    args.push('-ldflags=-H=windowsgui');
  }
  args.push('-o', path.join('build', name + suffix), './cmd/' + name);
  run(go, args);
}
const outputs = names.map(name => ({
  ...fileEvidence(root, path.join('build', name + suffix)),
  signature: 'not assessed; build performs no signing',
}));
const sourcesAfter = snapshotSources(root);
assertMatchingSources(sources, sourcesAfter);
writeBuildManifest(root, {
  schemaVersion: 1,
  purpose: 'Local build provenance, not a safety verdict, signature verification, or evidence about historical binaries.',
  startedAt, completedAt: new Date().toISOString(),
  build: { args: process.argv.slice(2), checks: process.argv.includes('--check'), commands },
  toolchain,
  source: {
    ...sources, scope: sourceScope, git,
    captured: 'After frontend generation, before Go checks/builds',
    afterGoSha256: sourcesAfter.sha256, unchangedAcrossGoBuild: true,
    consistency: 'Matching before/after snapshots; files were not locked against concurrent changes.',
  },
  outputs,
});
console.log('MirrorBird GUI, shim, and diagnostic builds are ready in build/.');
console.log('Local build provenance written to build/build-manifest.json (signatures not assessed).');
