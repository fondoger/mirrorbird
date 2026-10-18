import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, readdirSync, realpathSync, renameSync, writeFileSync } from 'node:fs';
import path from 'node:path';

const excludedDirectories = ['node_modules', 'build', 'dist', 'coverage', 'cache', 'caches', '__pycache__', 'tmp', 'temp', 'secrets', 'credentials'];
const excludedFiles = /(?:\.(?:log|tmp|tsbuildinfo|pem|key|p12|pfx|jks|keystore|kdbx)$|~$|^(?:credentials?|secrets?|tokens?)(?:[._-].*)?\.(?:json|ya?ml|toml|ini|txt)$|^auth\.json$|^id_(?:rsa|dsa|ecdsa|ed25519)(?:\.pub)?$)/i;
export const sourceScope = {
  directories: ['cmd', 'internal', 'tests', 'tools', 'web'],
  rootFiles: ['go.mod', 'go.sum', 'go.work', 'go.work.sum', 'mygo.json', 'CHANGELOG.md', 'THIRD_PARTY_NOTICES.md', '.github/workflows/ci.yml', '.github/workflows/release.yml'],
  excludedDirectories,
  excludedNamePrefix: '.', // Includes .git, .local, .tasks, .env*, and hidden caches/config.
  excludedFilePattern: excludedFiles.source,
  dependencies: 'Repository inputs only; dependency manifests are hashed, external dependencies/toolchain contents are not.',
};

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex');
}

function hashFile(filename) {
  const bytes = readFileSync(filename);
  return { sizeBytes: bytes.length, sha256: sha256(bytes) };
}

function outsideRoot(relative) {
  return relative === '..' || relative.startsWith('..' + path.sep) || path.isAbsolute(relative);
}

export function fileEvidence(root, relative) {
  if (path.isAbsolute(relative) || outsideRoot(path.normalize(relative))) {
    throw new Error('Evidence files must be relative to the repository.');
  }
  const filename = path.join(root, relative);
  if (!lstatSync(filename).isFile() || outsideRoot(path.relative(realpathSync(root), realpathSync(filename)))) {
    throw new Error(`Evidence input is not a regular repository file: ${relative}`);
  }
  return { path: relative.split(path.sep).join('/'), ...hashFile(filename) };
}

export function snapshotSources(root) {
  const files = [];
  function walk(relative, entry) {
    if (entry.isSymbolicLink()) throw new Error(`Source evidence does not follow symlinks: ${relative}`);
    if (entry.isDirectory()) {
      for (const child of readdirSync(path.join(root, relative), { withFileTypes: true })) {
        if (child.name.startsWith('.') || excludedFiles.test(child.name)) continue;
        if (child.isDirectory() && excludedDirectories.includes(child.name.toLowerCase())) continue;
        walk(path.join(relative, child.name), child);
      }
    } else {
      files.push(fileEvidence(root, relative));
    }
  }
  for (const relative of [...sourceScope.rootFiles, ...sourceScope.directories]) {
    let entry;
    try {
      entry = lstatSync(path.join(root, relative));
    } catch (error) {
      if (error.code === 'ENOENT') continue;
      throw error;
    }
    walk(relative, entry);
  }
  files.sort((a, b) => a.path < b.path ? -1 : a.path > b.path ? 1 : 0);
  // Only sorted paths, byte sizes and content hashes contribute, never timestamps.
  return { sha256: sha256(JSON.stringify(files)), files };
}

export function assertMatchingSources(before, after) {
  if (before.sha256 !== after.sha256) {
    throw new Error('Source inputs changed during Go checks/builds; refusing to write build evidence.');
  }
}

function capture(command, args, cwd) {
  const result = spawnSync(command, args, { cwd, encoding: 'utf8', windowsHide: true, env: process.env });
  return result.error || result.status !== 0 ? null : result.stdout;
}

export function gitEvidence(root, probe = capture) {
  const status = probe('git', ['--no-optional-locks', 'status', '--porcelain=v1', '-z', '--untracked-files=all'], root);
  if (status === null) return { available: false, revision: null, dirty: null, trackedDirty: null, untracked: null };
  const revision = probe('git', ['rev-parse', '--verify', 'HEAD'], root)?.trim() || null;
  let trackedCount = 0;
  let untrackedCount = 0;
  const entries = status.split('\0');
  for (let i = 0; i < entries.length; i++) {
    if (!entries[i]) continue;
    const code = entries[i].slice(0, 2);
    if (code === '??') untrackedCount++;
    else trackedCount++;
    if (/[RC]/.test(code)) i++; // Renames/copies have a second NUL-delimited path.
  }
  // Counts/booleans deliberately avoid publishing paths of unrelated untracked files.
  return { available: true, revision, dirty: trackedCount + untrackedCount > 0, trackedDirty: trackedCount > 0,
    untracked: untrackedCount > 0, untrackedCount };
}

export function toolchainEvidence(root, go, probe = capture) {
  const version = probe(go, ['version'], root)?.trim();
  const envJSON = probe(go, ['env', '-json', 'GOOS', 'GOARCH', 'CGO_ENABLED', 'GOTOOLCHAIN', 'GOFLAGS', 'GOROOT'], root);
  if (!version || envJSON === null) throw new Error('Could not read Go toolchain metadata for build evidence.');
  const env = JSON.parse(envJSON);
  let executable = { status: 'unresolved' };
  try {
    // GOROOT identifies the selected toolchain, including GOTOOLCHAIN auto selection.
    const filename = realpathSync(path.join(env.GOROOT, 'bin', process.platform === 'win32' ? 'go.exe' : 'go'));
    executable = { status: 'resolved', path: filename, ...hashFile(filename) };
  } catch {
    // Version/settings are still useful when the driver cannot be resolved or read.
  }
  const flags = env.GOFLAGS || '';
  return {
    node: { version: process.version },
    go: {
      version, executable, executableSource: 'Selected Go driver from go env GOROOT/bin/go; not the entire toolchain.',
      settings: {
        GOOS: env.GOOS, GOARCH: env.GOARCH, CGO_ENABLED: env.CGO_ENABLED, GOTOOLCHAIN: env.GOTOOLCHAIN,
        GOFLAGS: { set: flags.length > 0, sha256: sha256(flags), value: flags ? 'omitted: may contain sensitive flags or paths' : '' },
      },
    },
  };
}

export function writeBuildManifest(root, manifest) {
  const target = path.join(root, 'build', 'build-manifest.json');
  const temporary = target + '.tmp';
  writeFileSync(temporary, JSON.stringify(manifest, null, 2) + '\n', { encoding: 'utf8', flag: 'wx' });
  renameSync(temporary, target);
}
