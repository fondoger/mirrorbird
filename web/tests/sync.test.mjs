import test from 'node:test';
import assert from 'node:assert/strict';
import { HttpApi } from '../src/transport.ts';
import { parseSync } from '../src/sync.ts';
import { SnapshotMemo } from '../src/types.ts';

const snapshot = () => ({
  revision: 1, historyVersion: 1, pending: [], queue: [],
  thread: { id: 'a', name: 'A', cwd: '', status: { type: 'active' }, canAcceptDirectInput: true, turns: [
    { id: 'old', status: 'completed', items: [{ id: 'old-answer', type: 'agentMessage', text: 'old text' }] },
    { id: 'live', status: 'inProgress', items: [
      { id: 'user', type: 'userMessage', content: [{ type: 'text', text: 'hello' }] },
      { id: 'answer', type: 'agentMessage', text: 'hello' },
      { id: 'tool', type: 'commandExecution', aggregatedOutput: 'line\n' },
    ] },
  ] },
});
const full = (raw = snapshot(), cursor = 'c1') => ({ mode: 'snapshot', cursor, snapshot: raw, resetReason: 'initial' });
const delta = (patch, baseCursor = 'c1', cursor = 'c2') => ({ mode: 'delta', baseCursor, cursor, patch });
const json = (value) => new Response(JSON.stringify(value), { headers: { 'Content-Type': 'application/json' } });
const freeze = (value) => {
  if (value && typeof value === 'object') { Object.values(value).forEach(freeze); Object.freeze(value); }
  return value;
};

test('text and output patches share unchanged turns/items without mutating the base', () => {
  const memo = new SnapshotMemo(), raw = freeze(snapshot()), base = parseSync(full(raw), 'a', undefined, memo);
  const next = parseSync(delta([
    { op: 'append', path: ['thread', 'turns', 1, 'items', 1, 'text'], value: ' 😀\n<safe>' },
    { op: 'append', path: ['thread', 'turns', 1, 'items', 2, 'aggregatedOutput'], value: 'next\n' },
    { op: 'set', path: ['revision'], value: 2 },
  ]), 'a', base, memo);
  assert.equal(next.snapshot.thread.turns[0], base.snapshot.thread.turns[0]);
  assert.equal(next.snapshot.thread.turns[1].items[0], base.snapshot.thread.turns[1].items[0]);
  assert.equal(next.snapshot.thread.turns[1].items[1].text, 'hello 😀\n<safe>');
  assert.equal(next.snapshot.thread.turns[1].items[2].aggregatedOutput, 'line\nnext\n');
  assert.equal(raw.thread.turns[1].items[1].text, 'hello');
  assert.equal(base.snapshot.thread.turns[1].items[2].aggregatedOutput, 'line\n');
  const unchanged = parseSync(delta([], 'c2', 'c3'), 'a', next, memo);
  assert.equal(unchanged.snapshot, next.snapshot);
  assert.equal(unchanged.raw, next.raw);
});

test('window pruning, new items, metadata, approvals, queue and removals are incremental', () => {
  const memo = new SnapshotMemo(), base = parseSync(full(), 'a', undefined, memo);
  const next = parseSync(delta([
    { op: 'splice', path: ['thread', 'turns'], value: { index: 0, deleteCount: 1, items: [] } },
    { op: 'splice', path: ['thread', 'turns'], value: { index: 1, deleteCount: 0, items: [{ id: 'next', status: 'inProgress', items: [] }] } },
    { op: 'splice', path: ['thread', 'turns', 1, 'items'], value: { index: 0, deleteCount: 0, items: [{ id: 'new', type: 'agentMessage', text: 'new' }] } },
    { op: 'set', path: ['thread', 'turns', 0, 'status'], value: 'completed' },
    { op: 'remove', path: ['thread', 'turns', 0, 'items', 2, 'aggregatedOutput'] },
    { op: 'set', path: ['thread', 'model'], value: 'model' },
    { op: 'set', path: ['thread', 'canAcceptDirectInput'], value: false },
    { op: 'set', path: ['pending'], value: [{ id: 'opaque-900719925474099312345', method: 'approval', params: { command: 'pwd' }, desktopOnly: true }] },
    { op: 'set', path: ['queue'], value: [{ id: 'q', status: 'failed', text: 'queued', error: 'state_changed' }] },
    { op: 'set', path: ['historyVersion'], value: 2 },
    { op: 'set', path: ['resyncRequired'], value: true },
    { op: 'set', path: ['revision'], value: 2 },
  ]), 'a', base, memo);
  assert.deepEqual(next.snapshot.thread.turns.map((turn) => turn.id), ['live', 'next']);
  assert.equal(next.snapshot.thread.turns[0].items[0], base.snapshot.thread.turns[1].items[0]);
  assert.equal(next.snapshot.thread.turns[0].items[2].aggregatedOutput, '');
  assert.equal(next.snapshot.thread.turns[1].items[0].text, 'new');
  assert.equal(next.snapshot.pending[0].id, 'opaque-900719925474099312345');
  assert.equal(next.snapshot.pending[0].desktopOnly, true);
  assert.equal(next.snapshot.queue[0].status, 'failed');
  assert.equal(next.snapshot.historyVersion, 2);
  assert.equal(next.snapshot.thread.canAcceptDirectInput, false);
  assert.equal(next.snapshot.resyncRequired, true);
});

test('invalid patches are atomic and cannot cross cursors, threads or object prototypes', () => {
  const memo = new SnapshotMemo(), base = parseSync(full(freeze(snapshot())), 'a', undefined, memo);
  const invalid = [
    { op: 'append', path: ['pending'], value: 'no' },
    { op: 'append', path: ['missing'], value: 'no' },
    { op: 'set', path: [], value: {} },
    { op: 'set', path: ['thread', '__proto__', 'polluted'], value: true },
    { op: 'set', path: ['constructor', 'prototype'], value: {} },
    { op: 'set', path: ['thread', 'turns', -1], value: {} },
    { op: 'set', path: ['thread', 'turns', 1.5], value: {} },
    { op: 'set', path: ['thread', 'turns', 500], value: {} },
    { op: 'remove', path: ['thread', 'turns', 0] },
    { op: 'splice', path: ['pending'], value: { index: 1, deleteCount: 0, items: [] } },
    { op: 'splice', path: ['pending'], value: { index: 0, deleteCount: 1, items: [] } },
    { op: 'splice', path: ['pending'], value: { index: 0, deleteCount: -1, items: [] } },
    { op: 'set', path: ['revision'], value: -1 },
    { op: 'set', path: ['revision'], value: 0 },
    { op: 'set', path: ['thread', 'id'], value: 'b' },
    { op: 'set', path: ['pending'], value: [{ id: 9007199254740992 }] },
    { op: 'set', path: ['revision'] },
    { op: 'unknown', path: ['revision'], value: 2 },
  ];
  for (const patch of invalid) {
    assert.throws(() => parseSync(delta([
      { op: 'append', path: ['thread', 'turns', 1, 'items', 1, 'text'], value: 'uncommitted' }, patch,
    ]), 'a', base, memo), undefined, JSON.stringify(patch));
    assert.equal(base.raw.thread.turns[1].items[1].text, 'hello');
  }
  assert.throws(() => parseSync(delta([], 'other'), 'a', base, memo));
  assert.throws(() => parseSync(delta([]), 'b', base, memo));
  assert.throws(() => parseSync(delta([]), 'a', undefined, memo));
  assert.throws(() => parseSync(delta(Array.from({ length: 2049 }, () => ({ op: 'set', path: ['revision'], value: 2 }))), 'a', base, memo));
  assert.equal({}.polluted, undefined);
});

test('transport uses explicit baselines, supports snapshot fallback and never retries requests itself', async () => {
  const calls = [], responses = [full(), delta([{ op: 'set', path: ['revision'], value: 2 }]), { ...full({ ...snapshot(), revision: 3 }, 'c3'), resetReason: 'baseline_missing' }, full(snapshot(), 'c4'), delta([], 'wrong')];
  const api = new HttpApi(async (path, init) => { calls.push({ path, init }); return json(responses.shift()); });
  let state = await api.sync('a', undefined);
  state = await api.sync('a', state);
  assert.equal(state.snapshot.revision, 2);
  state = await api.sync('a', state);
  assert.equal(state.snapshot.revision, 3);
  state = await api.sync('a', state, { resync: true });
  assert.equal(state.snapshot.revision, 1);
  await assert.rejects(api.sync('a', state), (error) => error.code === 'sync_mismatch' && !error.unknownOutcome);
  assert.deepEqual(calls.map((call) => call.path), [
    '/api/threads/a/sync', '/api/threads/a/sync?cursor=c1', '/api/threads/a/sync?cursor=c2',
    '/api/threads/a/sync?resync=true', '/api/threads/a/sync?cursor=c4',
  ]);
  assert(calls.every(({ init }) => init.method === 'GET' && init.cache === 'no-store'));
});
