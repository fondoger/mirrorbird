import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';

const [desktop, realCodex, shim = path.resolve('build/mirrorbird-codex.exe')] = process.argv.slice(2);
if (!desktop || !realCodex) throw new Error('Usage: node tools/start-desktop.mjs <ChatGPT.exe> <original codex.exe> [shim.exe]');
for (const file of [desktop, realCodex, shim]) {
  if (!path.isAbsolute(file) || !fs.statSync(file).isFile()) throw new Error('Every executable must be an existing absolute file path');
}
const home = path.join(process.env.LOCALAPPDATA, 'MirrorBird', 'development');
fs.mkdirSync(home, { recursive: true });
const log = fs.openSync(path.join(home, 'desktop-launch.log'), 'a');
const child = spawn(desktop, ['--remote-debugging-port=9229', '--remote-debugging-address=127.0.0.1', '--force-renderer-accessibility'], {
  env: { ...process.env, CODEX_CLI_PATH: shim, MIRRORBIRD_REAL_CODEX: realCodex, MIRRORBIRD_HOME: home },
  cwd: path.dirname(desktop), detached: true, stdio: ['ignore', log, log],
});
child.on('error', error => { console.error(error.message); process.exitCode = 1; });
child.unref();
fs.closeSync(log);
console.log(JSON.stringify({ requestedDesktopPid: child.pid, home, debug: 'loopback:9229' }));
