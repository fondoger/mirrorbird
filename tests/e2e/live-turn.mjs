// Operates only on the explicitly supplied disposable test thread.
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import readline from 'node:readline';

const [metadataDir, threadId] = process.argv.slice(2);
if (!metadataDir || !threadId) throw new Error('Usage: node tests/e2e/live-turn.mjs <attachments-dir> <test-thread-id>');
const candidates = fs.readdirSync(metadataDir).filter(f => f.endsWith('.json'));
if (candidates.length !== 1) throw new Error('Specify a directory with exactly one verified live attachment');
const meta = JSON.parse(fs.readFileSync(path.join(metadataDir, candidates[0]), 'utf8'));
const conn = net.connect(meta.pipe);
await new Promise((resolve, reject) => { conn.once('connect', resolve); conn.once('error', reject); });
const lines = readline.createInterface({ input: conn });
let next = 0, events = [];
const pending = new Map();
lines.on('line', line => {
  const p = JSON.parse(line);
  if (p.method === 'event') { events.push(p.params); return; }
  const callback = pending.get(p.id);
  if (callback) { pending.delete(p.id); callback(p); }
});
function call(method, params) {
  const id = String(++next);
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`${method} timed out; not replayed`)); }, 45000);
    pending.set(id, p => { clearTimeout(timer); p.error ? reject(new Error(p.error.message)) : resolve(p.result); });
    conn.write(JSON.stringify({ id, method, params }) + '\n');
  });
}
try {
  await call('hello', { capability: meta.capability });
  await call('events', {});
  const status = await call('status', {});
  if (!status.ready) throw new Error('Desktop not initialized');
  const loaded = await call('thread/loaded/list', {});
  if (!loaded.data.includes(threadId)) throw new Error('Refusing to resume an unloaded thread');
  const initial = await call('turn/start', { threadId, input: [{ type: 'text', text: 'MirrorBird interruption test only. Do not run tools or access files. Write a numbered list from 1 to 500 with one short bird fact on every line, continuing until explicitly stopped.', text_elements: [] }] });
  const turnId = initial.turn.id;
  await new Promise(resolve => setTimeout(resolve, 4000));
  const steered = await call('turn/steer', { threadId, expectedTurnId: turnId, input: [{ type: 'text', text: 'MIRRORBIRD_STEER_MARKER: this instruction is appended to the currently running turn. Continue the list; do not use tools.', text_elements: [] }] });
  if (steered.turnId !== turnId) throw new Error('Steer changed the active turn');
  const steerDeadline = Date.now() + 20000;
  const steerVisible = () => events.some(e => e.message.method === 'item/started' && e.message.params.item?.type === 'userMessage' && JSON.stringify(e.message.params.item.content).includes('MIRRORBIRD_STEER_MARKER'));
  while (Date.now() < steerDeadline && !steerVisible()) await new Promise(resolve => setTimeout(resolve, 100));
  await call('turn/interrupt', { threadId, turnId });
  const until = Date.now() + 10000;
  while (Date.now() < until && !events.some(e => e.message.method === 'turn/completed' && e.message.params.turn.id === turnId)) {
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  const completed = events.find(e => e.message.method === 'turn/completed' && e.message.params.turn.id === turnId);
  if (!completed || completed.message.params.turn.status !== 'interrupted') throw new Error('Original turn did not report interruption');
  console.log(JSON.stringify({ passed: true, enginePid: status.enginePid, shimPid: status.shimPid, threadId, turnId, steerSameTurn: true, steerUserEventObserved: steerVisible(), interrupted: true, liveEvents: events.length, noSecondAgent: true }, null, 2));
} finally {
  lines.close(); conn.destroy();
}
