import test from 'node:test';
import assert from 'node:assert/strict';
import { OfflineCache } from '../src/cache.ts';
import { CompanionStore } from '../src/store.ts';

const storage = () => {
  const values = new Map();
  return { values, getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) };
};
const workspace = () => ({
  threads: [{ id: 'a', name: 'cached', status: 'active' }],
  snapshots: [{
    thread: { id: 'a', name: 'cached', turns: Array.from({ length: 20 }, (_, n) => ({ id: `t${n}`, status: 'completed', items: [] })) },
    revision: 8, pending: [{ id: 'request', method: 'approval', params: { secret: 'approval-only' } }],
  }],
  drafts: [['a', 'draft']], selectedId: 'a', savedAt: Date.now(), uncertain: ['a'],
});

test('cache keeps a bounded read-only view, drafts and uncertainty, but not approval authority', () => {
  const backing = storage(), cache = new OfflineCache(backing);
  cache.save(workspace());
  const saved = cache.load();
  assert.equal(saved.snapshots[0].thread.turns.length, 8);
  assert.equal(saved.snapshots[0].thread.turns[0].id, 't12');
  assert.deepEqual(saved.snapshots[0].pending, []);
  assert.deepEqual(saved.drafts, [['a', 'draft']]);
  assert.deepEqual(saved.uncertain, ['a']);
  assert(![...backing.values.values()].join('').includes('approval-only'));
});

test('expired, malformed and oversized cache contents are not used', () => {
  const backing = storage(), cache = new OfflineCache(backing);
  cache.save(workspace());
  const key = [...backing.values.keys()][0];
  const value = JSON.parse(backing.getItem(key)); value.savedAt = Date.now() - 8 * 86400_000;
  backing.setItem(key, JSON.stringify(value));
  assert.equal(cache.load(), undefined);
  assert.equal(backing.getItem(key), null);
  backing.setItem(key, 'not json');
  assert.throws(() => cache.load());
  backing.setItem(key, 'x'.repeat(2_000_001));
  assert.throws(() => cache.load(), /过大/);
});

test('cache failures are visible and cached snapshots cannot authorize offline mutations', () => {
  const backing = storage(), cache = new OfflineCache(backing);
  cache.save(workspace());
  let writes = 0;
  const store = new CompanionStore({ mutate: () => writes++ }, { cache, events: () => { throw new Error(); }, uuid: () => 'uuid', pollInterval: 0 });
  store.offline();
  assert.equal(store.state.snapshot.thread.id, 'a');
  assert.equal(store.state.draft.text, 'draft');
  assert.equal(store.state.gate.kind, 'unknown');
  assert(store.mutationBlock());
  assert.equal(writes, 0);
  store.clearCache(); store.saveCache();
  assert.equal(backing.values.size, 0, 'clear must not immediately repopulate the cache');
  store.dispose();
  const failed = new CompanionStore({}, { cache: new OfflineCache({ getItem() { throw new Error('disabled'); } }), events: () => {}, uuid: () => 'id' });
  assert.match(failed.state.cacheError, /无法读取/);
  failed.dispose();
});
