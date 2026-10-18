// An isolated, no-model protocol smoke test. It does NOT prove Desktop mirroring.
import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';
import readline from 'node:readline';

const real = process.argv[2];
if (!real || !path.isAbsolute(real)) throw new Error('Pass the absolute original Codex executable');
const dataRoot = path.join(process.env.LOCALAPPDATA, 'MirrorBird', 'tests');
fs.mkdirSync(dataRoot, { recursive: true });
const root = fs.mkdtempSync(path.join(dataRoot, 'protocol-smoke-'));
const codexHome = path.join(root, 'codex');
fs.mkdirSync(codexHome);
const child = spawn(path.resolve('build/mirrorbird-codex.exe'), ['app-server'], {
  env: { ...process.env, MIRRORBIRD_HOME: root, MIRRORBIRD_REAL_CODEX: real, CODEX_HOME: codexHome },
  windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'],
});
const pending = new Map();
const lines = readline.createInterface({ input: child.stdout });
lines.on('line', line => {
  const message = JSON.parse(line);
  const key = JSON.stringify(message.id);
  if (pending.has(key)) { pending.get(key)(message); pending.delete(key); }
});
let stderr = '';
child.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-4000); });
async function request(id, method, params) {
  const key = JSON.stringify(id);
  const promise = new Promise((resolve, reject) => {
    const timer = setTimeout(() => { pending.delete(key); reject(new Error('protocol response timeout')); }, 15000);
    pending.set(key, value => { clearTimeout(timer); resolve(value); });
  });
  child.stdin.write(JSON.stringify({ id, method, params }) + '\n');
  const response = await promise;
  if (response.error) throw new Error(JSON.stringify(response.error));
  return response.result;
}
try {
  const initialized = await request(0, 'initialize', { clientInfo: { name: 'mirrorbird_protocol_smoke', title: 'MirrorBird protocol smoke', version: '0.1.0' }, capabilities: { experimentalApi: true } });
  child.stdin.write(JSON.stringify({ method: 'initialized', params: {} }) + '\n');
  const loaded = await request('desktop-list', 'thread/loaded/list', {});
  const metadataPaths = fs.readdirSync(path.join(root, 'attachments')).filter(f => f.endsWith('.json'));
  if (metadataPaths.length !== 1) throw new Error('expected one backend attachment');
  const metadata = JSON.parse(fs.readFileSync(path.join(root, 'attachments', metadataPaths[0]), 'utf8'));
  console.log(JSON.stringify({ passed: true, shimPid: child.pid, enginePid: metadata.enginePid, initialized: !!initialized, loadedThreads: loaded.data?.length ?? null, isolatedCodexHome: true }));
} catch (error) {
  console.error(error.message);
  console.error(stderr);
  process.exitCode = 1;
} finally {
  child.stdin.end();
  const timer = setTimeout(() => child.kill(), 5000);
  await new Promise(resolve => { if (child.exitCode !== null) resolve(); else child.on('exit', resolve); });
  clearTimeout(timer);
  lines.close();
}
