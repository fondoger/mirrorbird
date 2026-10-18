import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, utimesSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import {
  assertMatchingSources, fileEvidence, gitEvidence, snapshotSources, toolchainEvidence, writeBuildManifest,
} from './build-evidence.mjs';

function fixture(t) {
  const root = mkdtempSync(path.join(os.tmpdir(), 'mirrorbird-evidence-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const put = (relative, content = relative) => {
    const filename = path.join(root, relative);
    mkdirSync(path.dirname(filename), { recursive: true });
    writeFileSync(filename, content);
    return filename;
  };
  return { root, put };
}
const hash = value => createHash('sha256').update(value).digest('hex');

test('source inventory sorts relative paths and excludes private data and caches', t => {
  const { root, put } = fixture(t);
  const included = ['web/static/assets/app.js', 'web/src/main.ts', 'web/package-lock.json', 'web/package.json',
    'tools/build.mjs', 'tests/preview/main.go', 'internal/app.go', 'cmd/app/main.go', 'mygo.json', 'go.sum', 'go.mod'];
  for (const relative of included) put(relative);
  for (const relative of ['.local/sdk/go.go', '.tasks/log.json', '.git/config', 'node_modules/a.js',
    'build/build-manifest.json', 'docs/private.txt', 'web/node_modules/a.js', 'web/.env.local',
    'web/.cache/a.js', 'web/dist/a.js', 'web/src/.env', 'web/src/secret.key', 'web/src/credentials.json',
    'web/src/secrets/token.txt', 'tools/auth.json', 'tools/debug.log', 'web/tsconfig.tsbuildinfo']) put(relative, 'PRIVATE');
  const first = snapshotSources(root);
  assert.deepEqual(first.files.map(file => file.path), [...included].sort());
  for (const file of first.files) {
    assert.equal(file.sha256, hash(file.path));
    assert.equal(file.sizeBytes, Buffer.byteLength(file.path));
  }
  assert.equal(first.sha256, hash(JSON.stringify(first.files)));
  utimesSync(path.join(root, 'go.mod'), new Date(0), new Date(0));
  put('build/build-manifest.json', 'new build timestamp');
  assert.deepEqual(snapshotSources(root), first);
});

test('source mutation, additions and removals invalidate evidence', t => {
  const { root, put } = fixture(t);
  put('internal/app.go', 'before');
  const before = snapshotSources(root);
  assertMatchingSources(before, snapshotSources(root));
  put('internal/app.go', 'after');
  assert.throws(() => assertMatchingSources(before, snapshotSources(root)), /Source inputs changed/);
  put('internal/app.go', 'before');
  const added = put('internal/new.go');
  assert.throws(() => assertMatchingSources(before, snapshotSources(root)), /Source inputs changed/);
  rmSync(added);
  rmSync(path.join(root, 'internal/app.go'));
  assert.throws(() => assertMatchingSources(before, snapshotSources(root)), /Source inputs changed/);
});

test('evidence refuses symlinks and repository escapes', t => {
  const { root, put } = fixture(t);
  const external = fixture(t);
  external.put('data.txt');
  put('internal/app.go');
  assert.throws(() => fileEvidence(root, '../external.txt'), /relative to the repository/);
  assert.throws(() => fileEvidence(root, path.resolve(root, 'go.mod')), /relative to the repository/);
  try {
    symlinkSync(external.root, path.join(root, 'internal/linked'), process.platform === 'win32' ? 'junction' : 'dir');
  } catch (error) {
    if (['EPERM', 'EACCES'].includes(error.code)) return t.skip('Symlink creation is unavailable on this host.');
    throw error;
  }
  assert.throws(() => snapshotSources(root), /does not follow symlinks/);
  assert.throws(() => fileEvidence(root, 'internal/linked/data.txt'), /regular repository file/);
});

test('Git evidence distinguishes dirty/untracked, unborn HEAD and unavailable Git without listing paths', () => {
  const probe = (status, revision) => (_command, args) => args.includes('status') ? status : revision;
  assert.deepEqual(gitEvidence('.', probe('?? private.txt\0', null)), {
    available: true, revision: null, dirty: true, trackedDirty: false, untracked: true, untrackedCount: 1,
  });
  const renamed = gitEvidence('.', probe('R  new.go\0?? old.go\0 M app.go\0?? secret.json\0', 'abc123\n'));
  assert.equal(renamed.revision, 'abc123');
  assert.equal(renamed.trackedDirty, true);
  assert.equal(renamed.untrackedCount, 1);
  assert.equal(JSON.stringify(renamed).includes('secret.json'), false);
  assert.equal(gitEvidence('.', probe('', 'abc123')).dirty, false);
  assert.deepEqual(gitEvidence('.', probe(null, null)), {
    available: false, revision: null, dirty: null, trackedDirty: null, untracked: null,
  });
});

test('toolchain records allowlisted settings and hashes flags, never invoking a fixture executable', t => {
  const { root, put } = fixture(t);
  const driver = put(`sdk/bin/${process.platform === 'win32' ? 'go.exe' : 'go'}`, 'fixture, not executable');
  const env = {
    GOOS: 'windows', GOARCH: 'arm64', CGO_ENABLED: '0', GOTOOLCHAIN: 'auto',
    GOFLAGS: '-ldflags=-X=package.token=PRIVATE', GOROOT: path.join(root, 'sdk'), GOPROXY: 'https://PRIVATE@example.test',
  };
  const calls = [];
  const probe = (_command, args) => {
    calls.push(args);
    return args[0] === 'version' ? 'go version go1.27.1 windows/arm64\n' : JSON.stringify(env);
  };
  const metadata = toolchainEvidence(root, 'go', probe);
  assert.equal(metadata.node.version, process.version);
  assert.equal(metadata.go.version, 'go version go1.27.1 windows/arm64');
  assert.equal(metadata.go.executable.path, driver);
  assert.equal(metadata.go.executable.sha256, hash('fixture, not executable'));
  assert.equal(metadata.go.settings.GOFLAGS.sha256, hash(env.GOFLAGS));
  assert.equal(metadata.go.settings.GOFLAGS.set, true);
  assert.equal(JSON.stringify(metadata).includes('PRIVATE'), false);
  assert.deepEqual(calls, [['version'], ['env', '-json', 'GOOS', 'GOARCH', 'CGO_ENABLED', 'GOTOOLCHAIN', 'GOFLAGS', 'GOROOT']]);
  rmSync(driver);
  assert.equal(toolchainEvidence(root, 'go', probe).go.executable.status, 'unresolved');
  assert.throws(() => toolchainEvidence(root, 'go', () => null), /Could not read Go toolchain/);
});

test('local manifest preserves output bytes/hashes and stays outside source aggregate', t => {
  const { root, put } = fixture(t);
  put('go.mod');
  const sources = snapshotSources(root);
  put('build/mirrorbird.exe', Buffer.from([0, 1, 2, 255]));
  const output = fileEvidence(root, 'build/mirrorbird.exe');
  assert.equal(output.sizeBytes, 4);
  assert.equal(output.sha256, hash(Buffer.from([0, 1, 2, 255])));
  const manifest = { source: sources, outputs: [output], signature: 'not assessed' };
  writeBuildManifest(root, manifest);
  assert.deepEqual(JSON.parse(readFileSync(path.join(root, 'build/build-manifest.json'), 'utf8')), manifest);
  assert.deepEqual(snapshotSources(root), sources);
});
