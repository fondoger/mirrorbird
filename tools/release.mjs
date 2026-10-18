import { spawnSync } from 'node:child_process';
import { createHash, createPrivateKey, createPublicKey, verify } from 'node:crypto';
import { lstatSync, mkdirSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { gunzipSync } from 'node:zlib';

export const MYGO_VERSION = 'v0.5.5';
export const PLATFORMS = ['windows/amd64', 'windows/arm64'];
const HELPERS = ['mirrorbird-codex', 'mirrorbird-diagnose'];
const NOTICE = 'THIRD_PARTY_NOTICES.md';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const sha256 = data => createHash('sha256').update(data).digest();
const requireThat = (condition, message) => { if (!condition) throw new Error(message); };
const fsName = name => name.trim().replace(/[\x00-\x1f/\\:*?"<>|]/g, '-');
const slug = name => name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'app';

export function parseArgs(args) {
  const options = { platforms: [...PLATFORMS], verifyOnly: false, publish: false };
  const seen = new Set();
  for (let i = 0; i < args.length; i++) {
    const flag = args[i];
    requireThat(!seen.has(flag), `Repeated argument: ${flag}`);
    seen.add(flag);
    if (flag === '--verify-only') options.verifyOnly = true;
    else if (flag === '--publish') options.publish = true;
    else if (flag === '--platform') {
      options.platforms = (args[++i] || '').split(',');
      requireThat(options.platforms.every(p => PLATFORMS.includes(p)) &&
        new Set(options.platforms).size === options.platforms.length, 'Use windows/amd64 and/or windows/arm64.');
    } else throw new Error('Usage: node tools/release.mjs [--platform windows/amd64,windows/arm64] [--verify-only] [--publish]');
  }
  requireThat(!options.publish || options.platforms.length === 2, '--publish requires both Windows architectures.');
  return options;
}

function base64(text, size, label) {
  const value = typeof text === 'string' ? text.trim() : '';
  const bytes = Buffer.from(value, 'base64');
  requireThat(bytes.length === size && bytes.toString('base64') === value, `Invalid ${label}.`);
  return bytes;
}

function publicKey(text) {
  return createPublicKey({ key: Buffer.concat([
    Buffer.from('302a300506032b6570032100', 'hex'), base64(text, 32, 'updates.publicKey'),
  ]), format: 'der', type: 'spki' });
}

export function checkSigningKey(config, secret) {
  requireThat(typeof secret === 'string' && secret.trim(), 'MYGO_UPDATER_PRIVATE_KEY is required; unsigned releases are refused.');
  const bytes = base64(secret, 64, 'MYGO_UPDATER_PRIVATE_KEY');
  const key = createPrivateKey({ key: Buffer.concat([
    Buffer.from('302e020100300506032b657004220420', 'hex'), bytes.subarray(0, 32),
  ]), format: 'der', type: 'pkcs8' });
  const derived = createPublicKey(key).export({ format: 'der', type: 'spki' }).subarray(-32);
  requireThat(derived.equals(bytes.subarray(32)) && derived.equals(base64(config.updates.publicKey, 32, 'updates.publicKey')),
    'MYGO_UPDATER_PRIVATE_KEY does not match updates.publicKey.');
}

export function loadConfig(directory) {
  const config = JSON.parse(readFileSync(path.join(directory, 'mygo.json'), 'utf8'));
  requireThat(config.main === './cmd/mirrorbird', 'mygo.json main must be ./cmd/mirrorbird.');
  requireThat(typeof config.name === 'string' && fsName(config.name), 'mygo.json needs an app name.');
  requireThat(/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(config.version), 'Releases need a stable x.y.z version.');
  requireThat(config.updates?.github === 'fondoger/mirrorbird' && !config.updates.url && !config.updates.s3,
    'updates.github must be fondoger/mirrorbird (no alternate update host).');
  requireThat(!config.updates.tagPrefix || config.updates.tagPrefix === 'v', 'updates.tagPrefix must be v.');
  requireThat(config.updates.deltas === 0, 'Set updates.deltas to 0: this release publishes exactly six assets.');
  publicKey(config.updates.publicKey);
  config.out ||= 'dist';
  requireThat(typeof config.out === 'string' && !path.isAbsolute(config.out) &&
    config.out.split(/[/\\]/).every(part => part && part !== '.' && part !== '..') && !config.out.includes(':'),
  'mygo.json out must be a relative subdirectory.');
  const mod = readFileSync(path.join(directory, 'go.mod'), 'utf8').replace(/\/\/[^\n]*/g, '');
  const versions = [...mod.matchAll(/^\s*(?:require\s+)?github\.com\/egoist\/mygo\s+(v\S+)/gm)];
  requireThat(versions.length === 1 && versions[0][1] === MYGO_VERSION &&
    !/^\s*(?:replace\s+)?github\.com\/egoist\/mygo\b[^\n]*=>/m.test(mod),
  `go.mod must require github.com/egoist/mygo ${MYGO_VERSION} without replacement.`);
  return config;
}

export function checkTag(config, tag) {
  requireThat(tag === `v${config.version}`, 'Release tag must equal v + mygo.json version.');
}

export function artifactNames(config, platform) {
  requireThat(PLATFORMS.includes(platform), 'Unsupported release platform.');
  const target = platform.replace('/', '-');
  const name = fsName(config.name);
  return {
    target, executables: [name + '.exe', ...HELPERS.map(helper => helper + '.exe')],
    setup: `${name} Setup ${config.version} ${platform.split('/')[1]}.exe`,
    archive: `${slug(name)}-${config.version}-${target}.tar.gz`,
    manifest: `update-${target}.json`,
  };
}

// Read headers, never execute a release binary (cross-built targets need not run here).
export function peInfo(data) {
  requireThat(data.length >= 64 && data.toString('ascii', 0, 2) === 'MZ', 'Invalid PE DOS header.');
  const offset = data.readUInt32LE(60);
  requireThat(offset >= 64 && offset + 94 <= data.length && data.readUInt32LE(offset) === 0x4550, 'Invalid PE header.');
  const optionalSize = data.readUInt16LE(offset + 20);
  requireThat(optionalSize >= 70 && offset + 24 + optionalSize <= data.length, 'Truncated PE optional header.');
  const magic = data.readUInt16LE(offset + 24);
  requireThat(magic === 0x10b || magic === 0x20b, 'Invalid PE optional header.');
  return { machine: data.readUInt16LE(offset + 4), subsystem: data.readUInt16LE(offset + 92), magic };
}

// MyGo v0.5.5 emits simple USTAR for these short, top-level file names.
// Fail closed on PAX, links and directories rather than extracting anything.
export function tarFiles(compressed) {
  const tar = gunzipSync(compressed, { maxOutputLength: 1 << 30 });
  const files = new Map();
  const text = bytes => bytes.toString('utf8').split('\0')[0];
  const octal = bytes => {
    const value = text(bytes).trim();
    requireThat(/^[0-7]+$/.test(value), 'Invalid tar numeric field.');
    const number = parseInt(value, 8);
    requireThat(Number.isSafeInteger(number), 'Tar numeric field is too large.');
    return number;
  };
  let offset = 0;
  while (offset + 512 <= tar.length) {
    const header = tar.subarray(offset, offset + 512);
    if (header.every(byte => byte === 0)) {
      requireThat(tar.length >= offset + 1024 && tar.length % 512 === 0 &&
        tar.subarray(offset).every(byte => byte === 0), 'Invalid tar trailer.');
      return files;
    }
    const checksum = header.reduce((sum, byte, index) => sum + (index >= 148 && index < 156 ? 32 : byte), 0);
    requireThat(checksum === octal(header.subarray(148, 156)), 'Invalid tar checksum.');
    const name = text(header.subarray(0, 100));
    requireThat(header.toString('ascii', 257, 263) === 'ustar\0' &&
      (header[156] === 0 || header[156] === 48) && !text(header.subarray(345, 500)) &&
      (/^[^/\\:\x00-\x1f]+\.exe$/.test(name) || name === NOTICE) && !files.has(name.toLowerCase()),
    'Tar must contain unique regular top-level executables and notices only.');
    const size = octal(header.subarray(124, 136));
    const start = offset + 512;
    offset = start + Math.ceil(size / 512) * 512;
    requireThat(size > 0 && offset <= tar.length, 'Truncated tar entry.');
    files.set(name.toLowerCase(), { name, data: tar.subarray(start, start + size) });
  }
  throw new Error('Missing tar trailer.');
}

function regularFile(filename, limit = 1 << 30) {
  const info = lstatSync(filename);
  requireThat(info.isFile() && info.size > 0 && info.size <= limit, `Missing, empty or invalid artifact: ${path.basename(filename)}`);
  return readFileSync(filename);
}

export function verifyArtifacts(directory, config, platforms = PLATFORMS) {
  const assets = [];
  for (const platform of platforms) {
    const names = artifactNames(config, platform);
    const dir = path.join(directory, config.out, names.target);
    requireThat(lstatSync(dir).isDirectory(), `Not an artifact directory: ${names.target}`);
    const notices = config.resources?.includes(NOTICE) ? [NOTICE] : [];
    const expected = [...names.executables, ...notices, names.setup, names.archive, names.manifest].sort();
    requireThat(JSON.stringify(readdirSync(dir).sort()) === JSON.stringify(expected), `Unexpected or missing files in ${names.target}.`);
    const manifestData = regularFile(path.join(dir, names.manifest), 1 << 20);
    const manifest = JSON.parse(manifestData);
    const archive = regularFile(path.join(dir, names.archive));
    const url = `https://github.com/${config.updates.github}/releases/download/v${config.version}/${names.archive}`;
    requireThat(manifest.version === config.version && manifest.size === archive.length && manifest.url === url,
      `Invalid version/size/url in ${names.manifest}.`);
    requireThat(!manifest.deltas?.length && !manifest.previous?.length, 'Delta/previous references are not part of this release.');
    requireThat(verify(null, sha256(archive), publicKey(config.updates.publicKey), base64(manifest.signature, 64, 'update signature')),
      `Invalid Ed25519 signature in ${names.manifest}.`);
    const files = tarFiles(archive);
    requireThat(files.size === 3 + notices.length, 'Update tar must contain three executables and the configured notices.');
    for (const name of notices) {
      const data = regularFile(path.join(dir, name), 1 << 20);
      requireThat(files.get(name.toLowerCase())?.data.equals(data) && data.equals(regularFile(path.join(directory, name), 1 << 20)),
        'Third-party notices must match the source and update archive.');
    }
    const machine = platform === 'windows/amd64' ? 0x8664 : 0xaa64;
    for (const [index, name] of names.executables.entries()) {
      const data = regularFile(path.join(dir, name));
      const entry = files.get(name.toLowerCase());
      requireThat(entry?.name === name && entry.data.equals(data), `Archive and loose executable differ: ${name}`);
      const pe = peInfo(data);
      requireThat(pe.machine === machine && pe.magic === 0x20b && pe.subsystem === (index === 0 ? 2 : 3),
        `Wrong PE architecture or GUI/console subsystem: ${names.target}/${name}`);
    }
    const setup = regularFile(path.join(dir, names.setup));
    const installer = peInfo(setup);
    // NSIS's default launcher is x86 for BOTH payload architectures, not arm64/amd64.
    requireThat(installer.machine === 0x14c && installer.magic === 0x10b && installer.subsystem === 2, 'Invalid NSIS installer PE.');
    for (const [name, data] of [[names.setup, setup], [names.archive, archive], [names.manifest, manifestData]]) {
      // GitHub normalizes spaces in "MirrorBird Setup ...exe" to periods.
      assets.push({ name, remoteName: name.replace(/ +/g, '.'), path: path.join(dir, name), size: data.length, sha256: sha256(data).toString('hex') });
    }
  }
  return assets;
}

function cleanEnv(env) {
  const result = { ...env };
  for (const key of Object.keys(result)) {
    if (['MYGO_UPDATER_PRIVATE_KEY', 'GH_TOKEN', 'GITHUB_TOKEN'].includes(key.toUpperCase())) delete result[key];
  }
  return result;
}

// Capture subprocess output so even an error cannot echo the supplied secrets.
function run(command, args, { cwd, env, quiet = false, allowFailure = false }) {
  const result = spawnSync(command, args, { cwd, env, encoding: 'utf8', maxBuffer: 16 << 20, windowsHide: true });
  const redact = text => [env.MYGO_UPDATER_PRIVATE_KEY, env.GH_TOKEN, env.GITHUB_TOKEN]
    .filter(Boolean).reduce((safe, secret) => safe.split(secret.trim()).join('[REDACTED]'), text || '');
  if (!quiet) {
    process.stdout.write(redact(result.stdout));
    process.stderr.write(redact(result.stderr));
  }
  requireThat(!result.error && (allowFailure || result.status === 0), `${path.basename(command)} failed (exit ${result.status ?? 'unavailable'}).`);
  return result;
}

export function buildArtifacts(directory, config, platforms, env, execute = run) {
  checkSigningKey(config, env.MYGO_UPDATER_PRIVATE_KEY);
  const safeEnv = { ...cleanEnv(env), CGO_ENABLED: '0', MYGO_INSPECTOR: '0' };
  const go = env.MIRRORBIRD_GO || 'go';
  if (path.isAbsolute(go)) {
    const pathKey = Object.keys(safeEnv).find(key => key.toUpperCase() === 'PATH') || 'PATH';
    safeEnv[pathKey] = path.dirname(go) + path.delimiter + (safeEnv[pathKey] || '');
  }
  // Never touch build/*.exe: a local development instance may still be using them.
  for (const platform of platforms) {
    rmSync(path.join(directory, config.out, artifactNames(config, platform).target), { recursive: true, force: true });
  }
  for (const args of [['ci', '--ignore-scripts', '--no-audit', '--no-fund'], ['run', 'typecheck'], ['test'], ['run', 'build']]) {
    const windows = process.platform === 'win32';
    execute(windows ? 'cmd.exe' : 'npm', windows ? ['/d', '/s', '/c', `npm ${args.join(' ')}`] : args,
      { cwd: path.join(directory, 'web'), env: safeEnv });
  }
  for (const platform of platforms) {
    const resources = path.join(directory, 'resources', artifactNames(config, platform).target);
    rmSync(resources, { recursive: true, force: true });
    mkdirSync(resources, { recursive: true });
    for (const helper of HELPERS) {
      execute(go, ['build', '-trimpath', '-ldflags=-s -w -H=windows', '-o', path.join(resources, helper + '.exe'), './cmd/' + helper],
        { cwd: directory, env: { ...safeEnv, GOOS: 'windows', GOARCH: platform.split('/')[1] } });
    }
  }
  // The go-run driver must execute on the host; MyGo cross-compiles its own children.
  const hostEnv = { ...safeEnv, MYGO_UPDATER_PRIVATE_KEY: env.MYGO_UPDATER_PRIVATE_KEY };
  delete hostEnv.GOOS;
  delete hostEnv.GOARCH;
  execute(go, ['run', `github.com/egoist/mygo/cmd/mygo@${MYGO_VERSION}`, 'build', '-platform', platforms.join(','), '-skip-build-command'],
    { cwd: directory, env: hostEnv });
}

function github(directory, env) {
  return args => {
    const result = run('gh', args, { cwd: directory, env: { ...cleanEnv(env), GH_TOKEN: env.GH_TOKEN, GH_HOST: 'github.com' }, quiet: true, allowFailure: true });
    requireThat(result.status === 0, 'GitHub command failed; release was not published by this step.');
    return result.stdout;
  };
}

export function publishRelease(directory, config, gh) {
  // No GitHub request (including draft creation) precedes full local verification.
  const assets = verifyArtifacts(directory, config, PLATFORMS);
  const tag = `v${config.version}`;
  const repo = config.updates.github;
  const info = () => {
    // REST's tag endpoint misses drafts; GraphQL also resolves their pending tags.
    const [owner, name] = repo.split('/');
    const query = 'query($owner:String!,$name:String!,$tag:String!){repository(owner:$owner,name:$name){release(tagName:$tag){databaseId}}}';
    const response = JSON.parse(gh(['api', 'graphql', '-f', `query=${query}`, '-f', `owner=${owner}`, '-f', `name=${name}`, '-f', `tag=${tag}`]));
    requireThat(!response.errors && response.data?.repository, 'Could not inspect GitHub release; refusing to create a draft.');
    const release = response.data.repository.release;
    if (release === null) return null;
    requireThat(Number.isSafeInteger(release?.databaseId) && release.databaseId > 0, 'Invalid GitHub release ID.');
    return JSON.parse(gh(['api', `repos/${repo}/releases/${release.databaseId}`]));
  };
  const assertDraft = release => {
    requireThat(release?.draft === true && release.tag_name === tag, 'Published releases are immutable; refusing to overwrite this version.');
    requireThat(release.assets.every(asset => assets.some(local => local.remoteName === asset.name)), 'Draft contains unexpected assets; remove them before retrying.');
  };
  let release = info();
  if (release) assertDraft(release);
  else {
    const manifest = JSON.parse(readFileSync(assets.find(asset => asset.name === 'update-windows-amd64.json').path, 'utf8'));
    gh(['release', 'create', tag, '--repo', repo, '--verify-tag', '--draft', '--title', `${config.name} ${config.version}`,
      '--notes', manifest.notes || `${config.name} ${config.version}`]);
  }
  assertDraft(info());
  for (const asset of assets) {
    requireThat(sha256(regularFile(asset.path)).toString('hex') === asset.sha256, 'Artifacts changed after verification.');
  }
  // Exactly six named assets, no glob. --clobber is permitted only on a confirmed draft.
  gh(['release', 'upload', tag, ...assets.map(asset => asset.path), '--repo', repo, '--clobber']);
  release = info();
  assertDraft(release);
  requireThat(release.assets.length === 6 && assets.every(local => release.assets.some(remote =>
    remote.name === local.remoteName && remote.state === 'uploaded' && remote.size === local.size && remote.digest === `sha256:${local.sha256}`)),
  'Uploaded assets differ from verified files; keeping the release as a draft.');
  gh(['release', 'edit', tag, '--repo', repo, '--draft=false', '--prerelease=false', '--latest']);
  return assets;
}

export function main(args = process.argv.slice(2), directory = root, env = process.env) {
  const options = parseArgs(args);
  const config = loadConfig(directory);
  if (env.GITHUB_REF_TYPE === 'tag') checkTag(config, env.GITHUB_REF_NAME);
  if (options.publish) {
    requireThat(env.GH_TOKEN?.trim(), '--publish requires GH_TOKEN.');
    if (env.GITHUB_ACTIONS === 'true') {
      requireThat(env.GITHUB_REF_TYPE === 'tag', 'CI publishing requires a tag event.');
      checkTag(config, env.GITHUB_REF_NAME);
    }
  }
  if (!options.verifyOnly) {
    requireThat(process.platform === 'win32', 'Packaging requires Windows (NSIS installer); --verify-only works on any host.');
    buildArtifacts(directory, config, options.platforms, env);
  }
  const assets = options.publish ? publishRelease(directory, config, github(directory, env)) : verifyArtifacts(directory, config, options.platforms);
  console.log(`${options.publish ? 'Published' : 'Verified'} ${config.version}: ${assets.length} release assets. Updater signatures verified; Authenticode is not assessed.`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
