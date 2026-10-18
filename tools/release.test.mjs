import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { gzipSync } from 'node:zlib';
import {
  artifactNames, buildArtifacts, checkSigningKey, checkTag, loadConfig, main,
  MYGO_VERSION, parseArgs, peInfo, PLATFORMS, publishRelease, tarFiles, verifyArtifacts,
} from './release.mjs';

const digest = data => createHash('sha256').update(data).digest();
function keys() {
  const { publicKey, privateKey } = generateKeyPairSync('ed25519');
  const pub = publicKey.export({ type: 'spki', format: 'der' }).subarray(-32);
  const seed = privateKey.export({ type: 'pkcs8', format: 'der' }).subarray(-32);
  return { privateKey, pub: pub.toString('base64'), secret: Buffer.concat([seed, pub]).toString('base64') };
}
function pe(machine, subsystem, magic = 0x20b) {
  const data = Buffer.alloc(512);
  data.write('MZ');
  data.writeUInt32LE(128, 60);
  data.writeUInt32LE(0x4550, 128);
  data.writeUInt16LE(machine, 132);
  data.writeUInt16LE(240, 148);
  data.writeUInt16LE(magic, 152);
  data.writeUInt16LE(subsystem, 220);
  return data;
}
function tar(entries) {
  const blocks = [];
  for (const { name, data, type = '0' } of entries) {
    const header = Buffer.alloc(512);
    header.write(name);
    header.write('0000755\0', 100);
    header.write('0000000\0', 108);
    header.write('0000000\0', 116);
    header.write(data.length.toString(8).padStart(11, '0') + '\0', 124);
    header.write('00000000000\0', 136);
    header.fill(32, 148, 156);
    header.write(type, 156);
    header.write('ustar\0', 257);
    header.write('00', 263);
    const sum = header.reduce((a, b) => a + b, 0);
    header.write(sum.toString(8).padStart(6, '0') + '\0 ', 148);
    blocks.push(header, data, Buffer.alloc((512 - data.length % 512) % 512));
  }
  return Buffer.concat([...blocks, Buffer.alloc(1024)]);
}
function fixture(t, platforms = PLATFORMS) {
  const directory = mkdtempSync(path.join(os.tmpdir(), 'mirrorbird-release-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const key = keys();
  const config = { name: 'MirrorBird', version: '0.1.0', main: './cmd/mirrorbird', out: 'build',
    updates: { github: 'fondoger/mirrorbird', publicKey: key.pub, deltas: 0 } };
  const put = (file, data) => {
    const filename = path.join(directory, file);
    mkdirSync(path.dirname(filename), { recursive: true });
    writeFileSync(filename, data);
    return filename;
  };
  put('mygo.json', JSON.stringify(config));
  put('go.mod', `module example\n\ngo 1.27.1\n\nrequire (\n github.com/egoist/mygo ${MYGO_VERSION}\n)\n`);
  const artifacts = platform => {
    const names = artifactNames(config, platform);
    const dir = `${config.out}/${names.target}`;
    const entries = names.executables.map((name, index) => ({ name,
      data: pe(platform.endsWith('/arm64') ? 0xaa64 : 0x8664, index === 0 ? 2 : 3) }));
    const writeArchive = (customEntries = entries, signer = key.privateKey) => {
      const archive = gzipSync(tar(customEntries));
      put(`${dir}/${names.archive}`, archive);
      const manifest = { version: config.version, notes: 'First release', size: archive.length,
        url: `https://github.com/${config.updates.github}/releases/download/v${config.version}/${names.archive}`,
        signature: sign(null, digest(archive), signer).toString('base64') };
      put(`${dir}/${names.manifest}`, JSON.stringify(manifest));
      return manifest;
    };
    for (const entry of entries) put(`${dir}/${entry.name}`, entry.data);
    put(`${dir}/${names.setup}`, pe(0x14c, 2, 0x10b));
    writeArchive();
    return { names, dir, entries, writeArchive };
  };
  const built = platforms.map(artifacts);
  return { directory, key, config, put, built };
}
function mockGitHub(f, { existing = null, remoteChange, failUpload = false } = {}) {
  const calls = [];
  let release = existing;
  const gh = args => {
    calls.push(args);
    if (args[1] === 'graphql') return JSON.stringify({ data: { repository: { release: release ? { databaseId: 123 } : null } } });
    if (args[0] === 'api') return JSON.stringify(release);
    if (args[1] === 'create') release = { tag_name: 'v0.1.0', draft: true, assets: [] };
    if (args[1] === 'upload') {
      if (failUpload) throw new Error('simulated upload failure');
      release.assets = verifyArtifacts(f.directory, f.config).map(asset => ({ name: asset.remoteName, size: asset.size,
        state: 'uploaded', digest: `sha256:${asset.sha256}` }));
      remoteChange?.(release);
    }
    return '';
  };
  return { gh, calls };
}

test('CLI defaults to both architectures and rejects partial publication or unknown options', () => {
  assert.deepEqual(parseArgs([]), { platforms: PLATFORMS, verifyOnly: false, publish: false });
  assert.deepEqual(parseArgs(['--platform', 'windows/arm64', '--verify-only']), {
    platforms: ['windows/arm64'], verifyOnly: true, publish: false,
  });
  for (const args of [['--platform'], ['--platform', 'linux/amd64'], ['--platform', 'windows/amd64,windows/amd64'],
    ['--publish', '--platform', 'windows/arm64'], ['--upload'], ['--publish', '--publish']]) {
    assert.throws(() => parseArgs(args));
  }
  assert.equal(parseArgs(['--verify-only', '--publish']).publish, true);
});

test('configuration pins MyGo and checks repository, stable version, tag and update policy', t => {
  const f = fixture(t, []);
  assert.deepEqual(loadConfig(f.directory), f.config);
  checkTag(f.config, 'v0.1.0');
  assert.throws(() => checkTag(f.config, 'v0.2.0'), /tag/);
  assert.throws(() => checkTag(f.config, undefined), /tag/);
  for (const version of ['v0.5.4', 'v0.5.50']) {
    f.put('go.mod', `require github.com/egoist/mygo ${version}`);
    assert.throws(() => loadConfig(f.directory), /go.mod/);
  }
  f.put('go.mod', `require github.com/egoist/mygo ${MYGO_VERSION}\nreplace (\ngithub.com/egoist/mygo => ../other\n)`);
  assert.throws(() => loadConfig(f.directory), /replacement/);
  f.put('go.mod', `require github.com/egoist/mygo ${MYGO_VERSION}`);
  for (const change of [{ main: '.' }, { version: '../0.1.0' }, { version: '0.1.0-rc.1' }, { out: '..' },
    { updates: { ...f.config.updates, github: 'other/repo' } }, { updates: { ...f.config.updates, deltas: 3 } },
    { updates: { ...f.config.updates, publicKey: '' } }, { updates: { ...f.config.updates, tagPrefix: 'app-' } }]) {
    f.put('mygo.json', JSON.stringify({ ...f.config, ...change }));
    assert.throws(() => loadConfig(f.directory));
  }
});

test('private-key gate derives the public key from the seed and never discloses secrets', t => {
  const f = fixture(t, []);
  checkSigningKey(f.config, f.key.secret + '\n');
  for (const secret of [undefined, '', 'SENSITIVE-not-a-key', keys().secret]) {
    assert.throws(() => checkSigningKey(f.config, secret), error => {
      assert.equal(error.message.includes(secret || 'SENSITIVE'), false);
      return /required|Invalid|does not match/.test(error.message);
    });
  }
  const forged = Buffer.from(keys().secret, 'base64');
  Buffer.from(f.key.pub, 'base64').copy(forged, 32);
  assert.throws(() => checkSigningKey(f.config, forged.toString('base64')), /does not match/);
});

test('verification checks six named assets, both PE payload architectures and NSIS x86 launchers', t => {
  const f = fixture(t);
  const assets = verifyArtifacts(f.directory, f.config);
  assert.equal(assets.length, 6);
  assert.equal(assets[0].remoteName, 'MirrorBird.Setup.0.1.0.amd64.exe');
  assert.deepEqual(assets.map(a => a.name), [
    'MirrorBird Setup 0.1.0 amd64.exe', 'mirrorbird-0.1.0-windows-amd64.tar.gz', 'update-windows-amd64.json',
    'MirrorBird Setup 0.1.0 arm64.exe', 'mirrorbird-0.1.0-windows-arm64.tar.gz', 'update-windows-arm64.json',
  ]);
  assert.equal(assets.every(a => a.size > 0 && /^[a-f0-9]{64}$/.test(a.sha256)), true);
  main(['--verify-only', '--platform', 'windows/arm64'], f.directory, {});
});

test('configured third-party notices ship unchanged beside and inside each update', t => {
  const f = fixture(t, ['windows/arm64']);
  f.config.resources = ['THIRD_PARTY_NOTICES.md'];
  const data = Buffer.from('Third-party licenses fixture\n');
  f.put('THIRD_PARTY_NOTICES.md', data);
  const b = f.built[0];
  f.put(`${b.dir}/THIRD_PARTY_NOTICES.md`, data);
  b.writeArchive([...b.entries, { name: 'THIRD_PARTY_NOTICES.md', data }]);
  assert.equal(verifyArtifacts(f.directory, f.config, ['windows/arm64']).length, 3);
  f.put('THIRD_PARTY_NOTICES.md', 'changed');
  assert.throws(() => verifyArtifacts(f.directory, f.config, ['windows/arm64']), /notices must match/);
});

test('manifest version, size, URL, signature and unuploaded delta references fail closed', t => {
  const f = fixture(t, ['windows/arm64']);
  const b = f.built[0];
  for (const change of [{ version: '0.2.0' }, { size: 1 }, { url: 'https://example.test/update.tar.gz' },
    { signature: Buffer.alloc(64).toString('base64') }, { deltas: [{ from: '0.0.9' }] }, { previous: [{}] }]) {
    const manifest = b.writeArchive();
    f.put(`${b.dir}/${b.names.manifest}`, JSON.stringify({ ...manifest, ...change }));
    assert.throws(() => verifyArtifacts(f.directory, f.config, ['windows/arm64']));
  }
  b.writeArchive(b.entries, keys().privateKey);
  assert.throws(() => verifyArtifacts(f.directory, f.config, ['windows/arm64']), /signature/);
});

test('signed tar still needs exactly the expected top-level bytes and correct GUI/console PE', t => {
  const f = fixture(t, ['windows/amd64']);
  const b = f.built[0];
  const verify = () => verifyArtifacts(f.directory, f.config, ['windows/amd64']);
  for (const entries of [b.entries.slice(0, 2), [...b.entries, { name: 'extra.exe', data: pe(0x8664, 3) }],
    b.entries.map(e => ({ ...e, name: 'nested/' + e.name })), b.entries.map(e => ({ ...e, type: '2' }))]) {
    b.writeArchive(entries);
    assert.throws(verify);
  }
  b.writeArchive();
  f.put(`${b.dir}/${b.names.executables[1]}`, pe(0xaa64, 3));
  assert.throws(verify, /differ/);
  for (const data of [pe(0xaa64, 3), pe(0x8664, 2)]) {
    const entries = b.entries.map((entry, i) => i === 1 ? { ...entry, data } : entry);
    b.writeArchive(entries);
    f.put(`${b.dir}/${b.names.executables[1]}`, data);
    assert.throws(verify, /PE architecture/);
  }
  f.put(`${b.dir}/${b.names.executables[1]}`, b.entries[1].data);
  b.writeArchive();
  f.put(`${b.dir}/private.key`, 'fixture secret');
  assert.throws(verify, /Unexpected/);
  rmSync(path.join(f.directory, b.dir, 'private.key'));
  rmSync(path.join(f.directory, b.dir, b.names.setup));
  assert.throws(verify, /missing/);
});

test('tar parser rejects corruption, traversal, duplicate paths, unsupported types and truncation', () => {
  const entry = { name: 'app.exe', data: pe(0x8664, 2) };
  assert.equal(tarFiles(gzipSync(tar([entry]))).get('app.exe').name, 'app.exe');
  for (const entries of [[{ ...entry, name: '../app.exe' }], [entry, entry], [{ ...entry, type: 'x' }]]) {
    assert.throws(() => tarFiles(gzipSync(tar(entries))));
  }
  const badChecksum = tar([entry]);
  badChecksum[0] ^= 1;
  for (const bytes of [badChecksum, tar([entry]).subarray(0, 700), tar([entry]).subarray(0, 1024),
    Buffer.concat([tar([entry]), Buffer.from('trailing data')])]) assert.throws(() => tarFiles(gzipSync(bytes)));
  assert.throws(() => peInfo(Buffer.from('not a PE')));
  const badPE = pe(0x8664, 2);
  badPE.writeUInt32LE(0xffffffff, 60);
  assert.throws(() => peInfo(badPE), /PE header/);
});

test('build plan checks frontend then console helpers then pinned MyGo, keeping secrets scoped', t => {
  const f = fixture(t);
  const sentinel = f.put('build/mirrorbird.exe', 'running old development GUI');
  f.put('resources/windows-arm64/stale.exe', 'stale');
  const calls = [];
  const execute = (command, args, options) => calls.push({ command, args, ...options });
  const env = { MYGO_UPDATER_PRIVATE_KEY: f.key.secret, GH_TOKEN: 'TOKEN', GITHUB_TOKEN: 'OTHER',
    GOOS: 'windows', GOARCH: 'arm64' };
  buildArtifacts(f.directory, f.config, ['windows/arm64'], env, execute);
  assert.equal(calls.length, 7);
  assert.match(calls[0].args.join(' '), /ci --ignore-scripts --no-audit --no-fund/);
  for (const [i, expected] of [[1, 'typecheck'], [2, 'test'], [3, 'build']]) assert.match(calls[i].args.join(' '), new RegExp(expected));
  for (const call of calls.slice(4, 6)) {
    assert.equal(call.env.GOARCH, 'arm64');
    assert.equal(call.env.GOOS, 'windows');
    assert.equal(call.env.CGO_ENABLED, '0');
    assert.equal(call.args.includes('-ldflags=-s -w -H=windows'), true);
    assert.match(call.args[4].replaceAll('\\', '/'), /resources\/windows-arm64\/mirrorbird-(codex|diagnose)\.exe$/);
  }
  const last = calls.at(-1);
  assert.deepEqual(last.args, ['run', 'github.com/egoist/mygo/cmd/mygo@v0.5.5', 'build', '-platform', 'windows/arm64', '-skip-build-command']);
  assert.equal(last.env.MYGO_UPDATER_PRIVATE_KEY, f.key.secret);
  assert.equal(last.env.GOARCH, undefined);
  assert.equal(calls.slice(0, -1).every(call => call.env.MYGO_UPDATER_PRIVATE_KEY === undefined), true);
  assert.equal(calls.every(call => call.env.GH_TOKEN === undefined && call.env.GITHUB_TOKEN === undefined), true);
  assert.equal(readFileSync(sentinel, 'utf8'), 'running old development GUI');
  assert.equal(existsSync(path.join(f.directory, 'build/windows-amd64')), true);
  assert.equal(existsSync(path.join(f.directory, 'build/windows-arm64')), false);
  assert.equal(existsSync(path.join(f.directory, 'resources/windows-arm64/stale.exe')), false);
  assert.throws(() => buildArtifacts(f.directory, f.config, PLATFORMS, {}, execute), /required/);
});

test('publication verifies both platforms before any request, including draft creation', t => {
  const f = fixture(t);
  rmSync(path.join(f.directory, f.built[1].dir, f.built[1].names.setup));
  const { gh, calls } = mockGitHub(f);
  assert.throws(() => publishRelease(f.directory, f.config, gh));
  assert.equal(calls.length, 0);
  assert.throws(() => main(['--verify-only', '--publish'], f.directory, {}), /GH_TOKEN/);
});

test('publication creates a draft, explicitly uploads six assets, checks remote hashes then marks latest', t => {
  const f = fixture(t);
  const { gh, calls } = mockGitHub(f);
  publishRelease(f.directory, f.config, gh);
  assert.deepEqual(calls.map(args => args[0] === 'api' ? args[1] === 'graphql' ? 'lookup' : 'get' : args[1]),
    ['lookup', 'create', 'lookup', 'get', 'upload', 'lookup', 'get', 'edit']);
  const create = calls.find(args => args[1] === 'create');
  assert.equal(create.includes('--draft'), true);
  assert.equal(create.includes('--verify-tag'), true);
  const upload = calls.find(args => args[1] === 'upload');
  assert.equal(upload.slice(3, 9).every(file => existsSync(file)), true);
  assert.equal(upload.length, 12);
  assert.deepEqual(calls.at(-1).slice(-3), ['--draft=false', '--prerelease=false', '--latest']);
});

test('draft reruns resume, published versions stay immutable, mismatches and upload failures remain drafts', t => {
  const f = fixture(t);
  const draft = () => ({ tag_name: 'v0.1.0', draft: true, assets: [{ name: f.built[0].names.setup.replaceAll(' ', '.') }] });
  const resumed = mockGitHub(f, { existing: draft() });
  publishRelease(f.directory, f.config, resumed.gh);
  assert.equal(resumed.calls.some(args => args[1] === 'create'), false);
  assert.equal(resumed.calls.some(args => args.includes('--clobber')), true);
  for (const options of [{ existing: { ...draft(), draft: false } },
    { existing: { ...draft(), assets: [{ name: 'unexpected.zip' }] } }, { failUpload: true },
    { remoteChange: r => { r.assets[0].digest = 'sha256:wrong'; } },
    { remoteChange: r => { r.assets[0].size++; } }, { remoteChange: r => { r.assets.pop(); } },
    { remoteChange: r => { r.draft = false; } }]) {
    const { gh, calls } = mockGitHub(f, options);
    assert.throws(() => publishRelease(f.directory, f.config, gh));
    assert.equal(calls.some(args => args[1] === 'edit'), false);
    if (options.existing) assert.equal(calls.length, 2);
  }
});
