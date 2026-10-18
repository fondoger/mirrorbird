// Runs only a version query: no login, model call, thread creation, or database writes.
import { spawn } from 'node:child_process';
import path from 'node:path';

const original = process.argv[2];
if (!original || !path.isAbsolute(original)) throw new Error('Pass the absolute original Codex executable');
const shim = path.resolve('build/mirrorbird-codex.exe');
const child = spawn(shim, ['--version'], {
  env: { ...process.env, MIRRORBIRD_REAL_CODEX: original },
  windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'],
});
let stdout = '', stderr = '';
child.stdout.on('data', chunk => { stdout += chunk; });
child.stderr.on('data', chunk => { stderr += chunk; });
const timer = setTimeout(() => child.kill(), 10000);
const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); });
clearTimeout(timer);
if (code !== 0 || !/^codex-cli \S+\s*$/.test(stdout)) throw new Error(`Shim version passthrough failed (${code}): ${stderr}`);
console.log(JSON.stringify({ passed: true, version: stdout.trim(), noProtocolLogsOnStdout: true }));
