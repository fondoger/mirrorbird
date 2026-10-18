import test from 'node:test';
import assert from 'node:assert/strict';
import { CompanionStore } from '../src/store.ts';
import { ApiError } from '../src/transport.ts';

const wait = (ms = 0) => new Promise((resolve) => setTimeout(resolve, ms));
const deferred = () => { let resolve; let reject; const promise = new Promise((res, rej) => { resolve = res; reject = rej; }); return { promise, resolve, reject }; };
const snapshot = (id = 'a', revision = 1, activeTurnId) => ({
  thread: { id, name: `会话 ${id}`, cwd: 'C:/repo', status: { type: activeTurnId ? 'active' : 'idle' }, turns: [], canAcceptDirectInput: true },
  revision, activeTurnId, pending: [],
});

class Stream {
  listeners = new Map();
  closed = false;
  addEventListener(type, callback) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(callback);
  }
  emit(type, data = {}) { for (const callback of this.listeners.get(type) ?? []) callback({ data: JSON.stringify(data) }); }
  close() { this.closed = true; }
}

function harness(overrides = {}, options = {}) {
  const calls = { connection: 0, threads: 0, queries: [], snapshot: [], reads: [], mutations: [], ids: 0 };
  const data = {
    connection: { sharing: true, connected: true, csrf: 'proof-1' },
    list: { connected: true, threads: ['a', 'b'].map((id) => ({ id, name: `会话 ${id}`, status: 'idle', archived: false })) },
    snapshot: (id) => snapshot(id),
  };
  const streams = [];
  const api = {
    connection: async (signal) => { calls.connection++; return overrides.connection ? overrides.connection(signal) : data.connection; },
    threads: async (query, signal) => { calls.threads++; calls.queries.push(query); return overrides.threads ? overrides.threads(query, signal) : data.list; },
    snapshot: async (id, options, signal) => { calls.snapshot.push(id); calls.reads.push({ id, ...options }); return overrides.snapshot ? overrides.snapshot(id, signal, options) : data.snapshot(id); },
    mutate: async (...args) => { calls.mutations.push(args); return overrides.mutate?.(...args); },
  };
  const store = new CompanionStore(api, {
    events: () => { const stream = new Stream(); streams.push(stream); return stream; },
    uuid: () => `random-uuid-${++calls.ids}`, syncDelay: 5, catalogueDelay: 5, pollInterval: 0, retryDelay: 1_000_000,
    ...options,
  });
  async function ready() {
    await store.start();
    streams.at(-1).emit('ready', { connected: true });
    await wait(10);
    assert.equal(store.state.link, 'live');
  }
  return { store, calls, data, streams, ready };
}

test('deliberate sends get fresh IDs; unknown outcomes preserve draft and require manual refresh', async (t) => {
  let fail = true;
  const h = harness({ mutate: async () => { if (fail) throw new ApiError(0, 'network_error', true); } });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('只发一次');
  await h.store.send();
  assert.equal(h.calls.mutations.length, 1);
  assert.equal(h.store.state.draft.text, '只发一次');
  assert.equal(h.store.state.gate.kind, 'unknown');
  await h.store.send();
  assert.equal(h.calls.mutations.length, 1, 'second click before refresh cannot post');
  await h.store.recover();
  h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.gate.kind, 'unknown', 'automatic reconciliation does not unlock');
  await h.store.refresh();
  assert.equal(h.store.state.gate, undefined);
  assert.match(h.store.state.notice.text, /核对/);
  assert.equal(h.store.state.draft.text, '只发一次');
  fail = false;
  await h.store.send();
  assert.equal(h.calls.mutations.length, 2);
  assert.notEqual(h.calls.mutations[0][3], h.calls.mutations[1][3]);
  assert.equal(h.store.state.draft.text, '');
});

test('manual refresh started before an uncertain send does not unlock its later failure', async (t) => {
  const post = deferred(); const read = deferred(); let delay = false;
  const h = harness({
    mutate: () => post.promise,
    snapshot: (id) => delay ? read.promise : Promise.resolve(snapshot(id)),
  });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('保留草稿'); const sending = h.store.send();
  delay = true; const refreshing = h.store.refresh(); await wait();
  post.reject(new ApiError(409, 'operation_rejected')); await sending;
  read.resolve(snapshot('a', 2)); await refreshing;
  assert.equal(h.store.state.gate.kind, 'rejected');
  assert.equal(h.store.state.draft.text, '保留草稿');
});

test('a GET begun before mutation completion cannot re-enable sending from stale state', async (t) => {
  const post = deferred(); const priorRead = deferred(); const afterRead = deferred();
  let phase = 'initial'; let usedPrior = false;
  const h = harness({
    mutate: () => post.promise,
    snapshot: (id) => {
      if (phase === 'initial') return Promise.resolve(snapshot(id));
      if (!usedPrior) { usedPrior = true; return priorRead.promise; }
      return afterRead.promise;
    },
  });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('first'); const sending = h.store.send();
  phase = 'mutation'; const reading = h.store.refresh(); await wait();
  post.resolve(); await sending;
  priorRead.resolve(snapshot('a', 1)); await reading; await wait();
  assert.equal(h.store.state.fresh, false, 'pre-operation read was retired');
  assert.equal(h.store.state.controlsFresh, false);
  assert.equal(h.store.state.snapshot.activeTurnId, undefined);
  h.store.setDraft('second'); await h.store.send();
  assert.equal(h.calls.mutations.length, 1);
  afterRead.resolve(snapshot('a', 2, 'new-turn')); await wait(10);
  assert.equal(h.store.state.snapshot.activeTurnId, 'new-turn');
});

test('stale snapshot for a previously selected thread never reaches the new thread', async (t) => {
  const a = deferred(); let delay = false;
  const h = harness({ snapshot: (id) => delay && id === 'a' ? a.promise : Promise.resolve(snapshot(id)) });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('A 的草稿'); delay = true;
  const oldRead = h.store.refresh(); await wait();
  h.store.selectThread('b'); h.store.setDraft('B 的草稿');
  a.resolve(snapshot('a', 20)); await oldRead; await wait(10);
  assert.equal(h.store.state.selectedId, 'b');
  assert.equal(h.store.state.snapshot.thread.id, 'b');
  assert.equal(h.store.state.draft.text, 'B 的草稿');
  h.store.selectThread('a'); await wait(10);
  assert.equal(h.store.state.draft.text, 'A 的草稿');
});

test('older revisions are rejected within a generation, but reconnect accepts a new low revision', async (t) => {
  const h = harness(); t.after(() => h.store.dispose());
  h.data.snapshot = (id) => snapshot(id, 20); await h.ready();
  h.data.snapshot = (id) => snapshot(id, 19);
  await h.store.refresh();
  assert.equal(h.store.state.snapshot.revision, 20);
  assert.equal(h.store.state.fresh, false);
  h.data.snapshot = (id) => snapshot(id, 1);
  await h.store.recover();
  h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.snapshot.revision, 1);
  assert.equal(h.store.state.link, 'live');
});

test('remote Stop clears snapshots and CSRF but never falsely resolves an admitted operation', async (t) => {
  const post = deferred();
  const h = harness({ mutate: () => post.promise }); t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('keep draft'); const sending = h.store.send();
  h.data.connection = { sharing: false, connected: true };
  await h.store.refresh();
  assert.equal(h.store.state.link, 'remote-off');
  assert.equal(h.store.state.controlsFresh, false);
  assert.equal(h.store.state.snapshot, undefined);
  assert.deepEqual(h.store.state.threads, []);
  assert.equal(h.store.state.selectedId, 'a');
  assert.equal(h.store.state.draft.text, 'keep draft');
  assert.equal(h.store.state.connection.csrf, undefined);
  assert.equal(h.store.state.gate.kind, 'unknown');
  post.resolve(); await sending;
  assert.equal(h.store.state.gate.kind, 'unknown', 'old success cannot erase the uncertainty');
  assert.equal(h.store.state.operation, undefined);
  h.data.connection = { sharing: true, connected: true, csrf: 'proof-2' };
  await h.store.recover(); h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.gate.kind, 'unknown', 'automatic restart does not review the old result');
  assert.equal(h.store.state.draft.text, 'keep draft');
  assert.equal(h.calls.mutations.length, 1);
  await h.store.refresh();
  assert.equal(h.store.state.gate, undefined);
  assert.equal(h.calls.mutations.length, 1, 'deliberate review is GET only');
});

test('repeated connection proof reads keep draft and gates; rotation fences old snapshots', async (t) => {
  const current = deferred(); let delay = false;
  const h = harness({
    mutate: async () => { throw new ApiError(0, 'network_error', true); },
    snapshot: (id) => delay ? current.promise : Promise.resolve(snapshot(id)),
  }); t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('previous lifetime'); await h.store.send(); await h.store.recover();
  h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  const old = h.streams.at(-1);
  delay = true;
  h.data.connection = { ...h.data.connection, csrf: 'new-generation-proof' };
  const recovering = h.store.recover(); await wait();
  assert.equal(h.store.state.snapshot, undefined);
  assert.equal(h.store.state.controlsFresh, false);
  assert.equal(h.store.state.draft.text, 'previous lifetime');
  assert.equal(h.store.state.connection.csrf, 'new-generation-proof');
  old.emit('ready', { connected: true }); old.emit('change', { threadId: 'a', revision: 100 });
  current.resolve(snapshot('a', 2)); await recovering;
  h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.gate.kind, 'unknown');
  assert.equal(h.store.state.snapshot.revision, 2);
  assert.equal(h.calls.mutations.length, 1);
});

test('SSE invalidations are coalesced and reads never overlap', async (t) => {
  let inFlight = 0; let maximum = 0; let delay = false;
  const h = harness({ snapshot: async (id) => {
    inFlight++; maximum = Math.max(maximum, inFlight);
    if (delay) await wait(15);
    inFlight--; return snapshot(id);
  } }, { syncDelay: 20 });
  t.after(() => h.store.dispose()); await h.ready(); delay = true;
  const before = h.calls.snapshot.length; const lists = h.calls.threads;
  for (let i = 0; i < 100; i++) h.streams.at(-1).emit('change', { threadId: 'a', revision: i });
  await wait(80);
  assert.ok(h.calls.snapshot.length - before <= 2, 'burst should not trigger per-token snapshots');
  assert.equal(maximum, 1);
  assert.equal(h.calls.threads, lists, 'transcript invalidations never refresh the catalogue');
});

test('continuous content events leave steer and interrupt usable while a GET lags behind', async (t) => {
  for (const action of ['steer', 'interrupt']) await t.test(action, async (t) => {
    const pendingRead = deferred(); const readStarted = deferred(); const post = deferred();
    let delayed = false; let reads = 0;
    const h = harness({
      snapshot: (id) => {
        if (!delayed) return Promise.resolve(snapshot(id, 1, 'turn-1'));
        if (++reads === 1) { readStarted.resolve(); return pendingRead.promise; }
        return Promise.resolve(snapshot(id, 2, 'turn-1'));
      },
      mutate: () => post.promise,
    }, { syncDelay: 20 });
    t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('keep working on this turn');
    assert.equal(h.store.state.controlsFresh, true);
    delayed = true;
    h.streams.at(-1).emit('change', { threadId: 'a', revision: 2, contentOnly: true });
    assert.equal(h.store.state.fresh, false);
    assert.equal(h.store.state.controlsFresh, true, 'a content event does not invalidate action state');
    await readStarted.promise;
    const before = h.calls.snapshot.length;
    for (let revision = 3; revision <= 8; revision++) {
      h.streams.at(-1).emit('change', { threadId: 'a', revision, contentOnly: true });
      await wait(2);
      assert.equal(h.store.state.controlsFresh, true);
      assert.equal(h.store.state.link, 'live', 'an ongoing content GET is not a connection reset');
      assert.equal(h.store.sendBlock(), '');
    }
    assert.equal(h.calls.snapshot.length, before, 'content bursts share the pending GET');
    pendingRead.resolve(snapshot('a', 2, 'turn-1')); await wait(30);
    assert.equal(h.store.state.snapshot.revision, 2);
    assert.equal(h.store.state.fresh, false, 'the transcript still lags revision 8');
    assert.equal(h.store.state.controlsFresh, true, 'lagging content does not regress controls on apply');
    const performing = action === 'steer' ? h.store.send() : h.store.interrupt();
    assert.equal(h.calls.mutations.length, 1);
    assert.deepEqual(h.calls.mutations[0][2], action === 'steer'
      ? { action: 'messages', text: 'keep working on this turn', mode: 'steer', expectedTurnId: 'turn-1' }
      : { action: 'interrupt', turnId: 'turn-1' });
    h.streams.at(-1).emit('change', { threadId: 'a', revision: 9, contentOnly: true });
    await h.store.send(); await h.store.interrupt();
    assert.equal(h.calls.mutations.length, 1, 'streaming never repeats an in-flight operation');
    post.resolve(); await performing; await wait(30);
    assert.equal(h.calls.mutations.length, 1, 'post-operation GETs never replay the command');
  });
});

test('a post-structural-event GET can restore controls before catching later content revisions', async (t) => {
  const current = deferred(); const started = deferred(); let delay = false;
  const h = harness({ snapshot: (id) => {
    if (!delay) return Promise.resolve(snapshot(id, 1));
    started.resolve(); return current.promise;
  } }, { syncDelay: 60_000 });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('supplement');
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 4 });
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 9, contentOnly: true });
  assert.equal(h.store.state.controlsFresh, false, 'content cannot repair a structural invalidation');
  delay = true; const refreshing = h.store.refresh(); await started.promise;
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 10, contentOnly: true });
  current.resolve(snapshot('a', 4, 'turn-1')); await refreshing;
  assert.equal(h.store.state.fresh, false);
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(h.store.state.link, 'live');
  assert.equal(h.store.state.draft.mode, 'start', 'a Desktop-started turn still needs a deliberate mode change');
  h.store.setMode('steer'); assert.equal(h.store.sendBlock(), '');
});

test('initial streaming snapshot initializes the draft from structurally fresh action state', async (t) => {
  const current = deferred(); const started = deferred(); let delay = false;
  const h = harness({ snapshot: (id) => {
    if (!delay) return Promise.resolve(snapshot(id, 1));
    started.resolve(); return current.promise;
  } }, { syncDelay: 60_000 });
  t.after(() => h.store.dispose()); await h.ready(); delay = true;
  h.streams.at(-1).emit('change', { threadId: 'b', revision: 2 });
  h.store.selectThread('b'); await started.promise;
  h.streams.at(-1).emit('change', { threadId: 'b', revision: 3, contentOnly: true });
  current.resolve(snapshot('b', 2, 'turn-b')); await wait(10);
  assert.equal(h.store.state.fresh, false);
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(h.store.state.draft.mode, 'steer');
  assert.equal(h.store.state.draft.targetTurnId, 'turn-b');
});

test('content-only bypass requires an exact boolean and valid scoped revision', async (t) => {
  const invalid = [
    { threadId: 'a', revision: 2 },
    { threadId: 'a', revision: 2, contentOnly: false },
    { threadId: 'a', revision: 2, contentOnly: 'true' },
    { threadId: 'a', revision: 2, contentOnly: 1 },
    { revision: 2, contentOnly: true },
    { threadId: '', revision: 2, contentOnly: true },
    { threadId: 1, revision: 2, contentOnly: true },
    { threadId: 'a', contentOnly: true },
    { threadId: 'a', revision: '2', contentOnly: true },
    { threadId: 'a', revision: -1, contentOnly: true },
    { threadId: 'a', revision: 2.5, contentOnly: true },
    { threadId: 'a', revision: Number.MAX_SAFE_INTEGER + 1, contentOnly: true },
    { threadId: 'a', revision: null, contentOnly: true },
  ];
  for (const data of invalid) await t.test(JSON.stringify(data), async (t) => {
    const h = harness({}, { syncDelay: 60_000 }); t.after(() => h.store.dispose()); await h.ready();
    h.store.setDraft('not yet');
    h.streams.at(-1).emit('change', data);
    assert.equal(h.store.state.fresh, false);
    assert.equal(h.store.state.controlsFresh, false);
    await h.store.send(); await h.store.interrupt();
    assert.equal(h.calls.mutations.length, 0);
  });
});

test('selected-thread event blocks mutations immediately, before a delayed refresh starts', async (t) => {
  const current = deferred(); const started = deferred();
  let delay = false;
  const approval = { id: 'opaque-pending', method: 'item/commandExecution/requestApproval', params: { command: 'go test ./...' } };
  const h = harness({ snapshot: (id) => {
    if (!delay) return Promise.resolve({ ...snapshot(id, 1, 'turn-1'), pending: [approval] });
    started.resolve(); return current.promise;
  } }, { syncDelay: 60_000 });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('补充检查'); assert.equal(h.store.sendBlock(), '');
  delay = true;
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 2 });
  assert.equal(h.store.state.fresh, false, 'event disables actions synchronously');
  assert.equal(h.store.state.controlsFresh, false);
  await h.store.send(); await h.store.interrupt(); await h.store.respond(approval.id, 'accept');
  assert.equal(h.calls.mutations.length, 0);
  const refreshing = h.store.refresh(); await started.promise;
  assert.equal(h.store.state.fresh, false, 'waiting snapshot stays nonfresh');
  current.resolve({ ...snapshot('a', 2, 'turn-1'), pending: [approval] }); await refreshing;
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.sendBlock(), '');
});

test('in-flight snapshots cannot restore freshness after a newer selected event, even at its revision', async (t) => {
  const old = deferred(); const oldStarted = deferred(); const current = deferred(); const currentStarted = deferred();
  let delay = false; let reads = 0; let signal;
  const h = harness({ snapshot: (id, readSignal) => {
    if (!delay) return Promise.resolve(snapshot(id));
    if (reads++ === 0) { signal = readSignal; oldStarted.resolve(); return old.promise; }
    currentStarted.resolve(); return current.promise;
  } });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('保留草稿');
  delay = true; const refreshing = h.store.refresh(); await oldStarted.promise;
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 2 });
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 3, contentOnly: true });
  assert.equal(signal.aborted, false, 'an event does not cancel an in-flight snapshot');
  old.resolve(snapshot('a', 2)); await refreshing; await currentStarted.promise;
  assert.equal(h.store.state.fresh, false, 'a pre-event read cannot authorize an operation');
  assert.equal(h.store.state.controlsFresh, false, 'the structural barrier still blocks a matching revision');
  await h.store.send(); assert.equal(h.calls.mutations.length, 0);
  current.resolve(snapshot('a', 3)); await wait(10);
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(h.store.state.draft.text, '保留草稿');
});

test('highest per-thread event revision blocks lagging reads and survives lower out-of-order events', async (t) => {
  const h = harness({}, { syncDelay: 60_000 }); t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('send after latest');
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 8 });
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 3 });
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 10, contentOnly: true });
  h.data.snapshot = (id) => snapshot(id, 7); await h.store.refresh();
  assert.equal(h.store.state.fresh, false, 'revision 7 cannot satisfy observed revision 8');
  assert.equal(h.store.state.controlsFresh, false, 'later content must not erase the structural watermark');
  await h.store.send(); assert.equal(h.calls.mutations.length, 0);
  h.data.snapshot = (id) => snapshot(id, 8); await h.store.refresh();
  assert.equal(h.store.state.fresh, false, 'transcript freshness still waits for revision 10');
  assert.equal(h.store.state.controlsFresh, true, 'the structural watermark does not chase content');
  assert.equal(h.store.sendBlock(), '');
});

test('other-thread events retain selected freshness and become a watermark when selected', async (t) => {
  const h = harness({}, { syncDelay: 60_000 }); t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('A stays ready');
  h.streams.at(-1).emit('change', { threadId: 'b', revision: 50 });
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.sendBlock(), '', 'unrelated thread does not disable selected mutations');
  h.store.selectThread('b'); await wait(10);
  assert.equal(h.store.state.selectedId, 'b');
  assert.equal(h.store.state.fresh, false, 'new selection must meet B event watermark');
  h.data.snapshot = (id) => snapshot(id, id === 'b' ? 50 : 1); await h.store.refresh();
  assert.equal(h.store.state.fresh, true);
});

test('unscoped invalidations block pre-event snapshots without comparing unrelated revision numbers', async (t) => {
  const old = deferred(); const started = deferred(); let delay = false;
  const h = harness({ snapshot: (id) => {
    if (!delay) return Promise.resolve(snapshot(id));
    started.resolve(); return old.promise;
  } }, { syncDelay: 60_000 });
  t.after(() => h.store.dispose()); await h.ready();
  delay = true; const refreshing = h.store.refresh(); await started.promise;
  h.streams.at(-1).emit('change', { revision: 900 });
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 2, contentOnly: true });
  assert.equal(h.store.state.fresh, false);
  assert.equal(h.store.state.controlsFresh, false);
  old.resolve(snapshot('a')); await refreshing;
  assert.equal(h.store.state.fresh, false);
  assert.equal(h.store.state.controlsFresh, false);
  delay = false; await h.store.refresh();
  assert.equal(h.store.state.fresh, false, 'content still lags revision 2');
  assert.equal(h.store.state.controlsFresh, true, 'unscoped revision is not a per-thread revision');
});

test('reconnect clears old event watermarks and accepts a new generation at a low revision', async (t) => {
  const h = harness({}, { syncDelay: 60_000 }); t.after(() => h.store.dispose()); await h.ready();
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 50 });
  assert.equal(h.store.state.fresh, false);
  await h.store.recover();
  h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.snapshot.revision, 1);
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.state.link, 'live');
});

test('a manual unknown-result review must meet the latest event watermark before unlocking', async (t) => {
  const old = deferred(); const started = deferred(); let delay = false; let revision = 1;
  const h = harness({
    mutate: async () => { throw new ApiError(0, 'network_error', true); },
    snapshot: (id) => { if (!delay) return Promise.resolve(snapshot(id, revision)); started.resolve(); return old.promise; },
  }, { syncDelay: 60_000 });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('only once'); await h.store.send();
  await h.store.recover(); h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  delay = true; const refreshing = h.store.refresh(); await started.promise;
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 2 });
  old.resolve(snapshot('a', 2)); await refreshing;
  assert.equal(h.store.state.gate.kind, 'unknown');
  assert.equal(h.store.state.fresh, false);
  // A deliberate review whose read still lags the watermark cannot unlock.
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 3 });
  await h.store.refresh();
  assert.equal(h.store.state.gate.kind, 'unknown');
  assert.equal(h.calls.mutations.length, 1);
  delay = false; revision = 3; await h.store.refresh();
  assert.equal(h.store.state.gate, undefined);
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.state.draft.text, 'only once');
});

test('content freshness still gates a deliberate unknown-result review', async (t) => {
  const old = deferred(); const started = deferred(); let delay = false; let revision = 1;
  const h = harness({
    mutate: async () => { throw new ApiError(0, 'network_error', true); },
    snapshot: (id) => { if (!delay) return Promise.resolve(snapshot(id, revision)); started.resolve(); return old.promise; },
  }, { syncDelay: 60_000 });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('only once'); await h.store.send();
  await h.store.recover(); h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.gate.kind, 'unknown', 'automatic reconciliation cannot unlock');
  delay = true; const refreshing = h.store.refresh(); await started.promise;
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 2, contentOnly: true });
  old.resolve(snapshot('a', 2)); await refreshing;
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(h.store.state.fresh, false, 'pre-content-event review remains insufficient even at its revision');
  assert.equal(h.store.state.gate.kind, 'unknown');
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 3, contentOnly: true });
  delay = false; revision = 2; await h.store.refresh();
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(h.store.state.fresh, false, 'manual review also has to meet the all-event watermark');
  assert.equal(h.store.state.gate.kind, 'unknown');
  await h.store.send(); assert.equal(h.calls.mutations.length, 1);
  revision = 3; await h.store.refresh();
  assert.equal(h.store.state.gate, undefined);
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.state.draft.text, 'only once');
  assert.equal(h.calls.mutations.length, 1, 'review does not replay');
});

test('content events cannot cross reset, disconnected, focus-recovery, or offline fences', async (t) => {
  for (const boundary of ['reset', 'disconnected', 'recover', 'offline']) await t.test(boundary, async (t) => {
    const current = deferred(); const started = deferred(); let delay = false;
    const h = harness({ snapshot: (id) => {
      if (!delay) return Promise.resolve(snapshot(id, 1, 'turn-1'));
      started.resolve(); return current.promise;
    } }, { syncDelay: 60_000 });
    t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('keep draft');
    const old = h.streams.at(-1); delay = true;
    if (boundary === 'recover') void h.store.recover();
    else if (boundary === 'offline') h.store.offline();
    else if (boundary === 'reset') old.emit('reset');
    else old.emit('change', { threadId: 'a', revision: 2, contentOnly: true, disconnected: true });
    old.emit('change', { threadId: 'a', revision: 3, contentOnly: true });
    assert.equal(h.store.state.controlsFresh, false);
    assert.notEqual(h.store.state.link, 'live');
    await h.store.send(); await h.store.interrupt();
    assert.equal(h.calls.mutations.length, 0);
    if (boundary === 'offline') void h.store.recover();
    await started.promise; current.resolve(snapshot('a', 1, 'turn-1')); await wait(10);
    assert.notEqual(h.store.mutationBlock(), '', 'a GET without a live replacement stream is insufficient');
    const replacement = h.streams.at(-1);
    replacement.emit('ready', { connected: true });
    replacement.emit('change', { threadId: 'a', revision: 2, contentOnly: true });
    assert.equal(h.store.state.controlsFresh, false, 'content cannot authorize during the ready fence');
    await wait(10);
    assert.equal(h.store.state.controlsFresh, true);
    assert.equal(h.store.state.link, 'live');
    assert.equal(h.store.state.fresh, false);
    assert.equal(h.store.sendBlock(), '');
  });
});

test('a stale EventSource cannot invalidate or revive the replacement stream', async (t) => {
  const h = harness(); t.after(() => h.store.dispose()); await h.ready();
  const old = h.streams[0];
  await h.store.recover();
  const replacement = h.streams.at(-1);
  old.emit('ready', { connected: false }); old.emit('reset');
  replacement.emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.link, 'live');
  assert.ok(old.closed);
});

test('start versus steer stays deliberate when Desktop changes active turns', async (t) => {
  const h = harness(); t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('追加检查');
  h.data.snapshot = (id) => snapshot(id, 2, 'turn-1'); await h.store.refresh();
  assert.equal(h.store.state.draft.mode, 'start');
  await h.store.send(); assert.equal(h.calls.mutations.length, 0);
  h.store.setMode('steer');
  assert.equal(h.store.state.draft.targetTurnId, 'turn-1');
  h.data.snapshot = (id) => snapshot(id, 3, 'turn-2'); await h.store.refresh();
  await h.store.send(); assert.equal(h.calls.mutations.length, 0);
  h.store.setMode('steer'); await h.store.send();
  assert.equal(h.calls.mutations[0][2].mode, 'steer');
  assert.equal(h.calls.mutations[0][2].expectedTurnId, 'turn-2');
});

test('projection recovery requires explicit resync GET and never sends a model operation', async (t) => {
  const h = harness({ snapshot: (id, _signal, options) => ({ ...snapshot(id, options.resync ? 2 : 1), resyncRequired: !options.resync }) });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('not safe');
  await h.store.send(); await h.store.refresh(); await h.store.send();
  assert.equal(h.calls.mutations.length, 0);
  assert.match(h.store.mutationBlock(), /重新同步/);
  assert.ok(h.calls.reads.every((read) => !read.resync));
  await h.store.resync();
  assert.equal(h.calls.reads.at(-1).resync, true);
  assert.equal(h.store.state.snapshot.resyncRequired, false);
  assert.equal(h.calls.mutations.length, 0);
});

test('success notice settles after a fresh post-operation snapshot', async (t) => {
  const h = harness(); t.after(() => h.store.dispose());
  h.data.snapshot = (id) => snapshot(id, 1, 'active'); await h.ready();
  await h.store.interrupt(); await wait(10);
  assert.equal(h.store.state.notice.text, 'Desktop 已接收操作，最新会话状态已同步。');
});

test('post success does not erase a newer draft typed while awaiting its response', async (t) => {
  const post = deferred(); const h = harness({ mutate: () => post.promise });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('first'); const sending = h.store.send();
  h.store.setDraft('second'); post.resolve(); await sending;
  assert.equal(h.store.state.draft.text, 'second');
});

test('approval response retains opaque string handle and unsupported methods cannot respond', async (t) => {
  const h = harness(); t.after(() => h.store.dispose());
  const id = 'opaque-90071992547409930001';
  h.data.snapshot = (thread) => ({ ...snapshot(thread), pending: [{ id, method: 'item/commandExecution/requestApproval', params: { command: 'go test ./...' } }] });
  await h.ready(); await h.store.respond(id, 'accept');
  assert.deepEqual(h.calls.mutations[0][2], { action: 'respond', id, decision: 'accept' });
  h.data.snapshot = (thread) => ({ ...snapshot(thread, 2), pending: [{ id, method: 'item/tool/requestUserInput', params: {} }] });
  await h.store.refresh(); await h.store.respond(id, 'accept');
  assert.equal(h.calls.mutations.length, 1);
  h.data.snapshot = (thread) => ({ ...snapshot(thread, 3), pending: [{ id, method: 'item/commandExecution/requestApproval', params: { command: 'go test ./...' }, desktopOnly: true }] });
  await h.store.refresh(); await h.store.respond(id, 'decline');
  assert.equal(h.calls.mutations.length, 1, 'Desktop-only requests cannot be answered even through the store');
});

test('bootstrap and reconnect automatically GET connection without registration state', async (t) => {
  const h = harness(); t.after(() => h.store.dispose()); await h.ready();
  assert.equal(h.store.state.selectedId, 'a');
  assert.equal(h.store.state.snapshot.thread.id, 'a');
  assert.equal(h.store.state.connection.csrf, 'proof-1');
  assert.equal('auth' in h.store.state, false);
  assert.equal('pairing' in h.store.state, false);
  const before = h.calls.connection;
  await h.store.recover();
  assert.ok(h.calls.connection > before);
  assert.equal(h.calls.mutations.length, 0);
});

test('catalogue-only SSE hints coalesce independently and never invalidate selected controls', async (t) => {
  const list = deferred(); const started = deferred(); let delay = false; let reads = 0;
  const h = harness({ threads: async () => {
    if (!delay) return { connected: true, threads: [{ id: 'a', name: 'A' }] };
    reads++; started.resolve(); return list.promise;
  } }, { catalogueDelay: 20 });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('selected stays ready');
  const before = h.calls.snapshot.length;
  delay = true;
  h.streams.at(-1).emit('change', { catalogueChanged: true, revision: 0 }); await started.promise;
  for (let i = 0; i < 100; i++) h.streams.at(-1).emit('change', { catalogueChanged: true, revision: i });
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(h.store.sendBlock(), '');
  list.resolve({ connected: true, threads: [{ id: 'b', name: 'new catalogue entry', canAcceptDirectInput: null }] });
  await wait(60);
  assert.ok(reads <= 2);
  assert.equal(h.calls.snapshot.length, before);
  assert.equal(h.store.state.threads[0].id, 'b');
  assert.equal(h.store.state.selectedId, 'a');
  assert.equal(h.store.state.snapshot.thread.id, 'a');
  assert.equal(h.store.state.draft.text, 'selected stays ready');
});

test('combined catalogue and selected change performs both without weakening structural fences', async (t) => {
  const h = harness({}, { syncDelay: 60_000, catalogueDelay: 0 }); t.after(() => h.store.dispose()); await h.ready();
  const before = h.calls.threads;
  h.streams.at(-1).emit('change', { catalogueChanged: true, threadId: 'a', revision: 9 });
  assert.equal(h.store.state.controlsFresh, false);
  await wait(10); assert.ok(h.calls.threads > before);
  h.data.snapshot = (id) => snapshot(id, 9); await h.store.refresh();
  assert.equal(h.store.state.controlsFresh, true);
});

test('content bursts and lagging snapshot retries never request the catalogue', async (t) => {
  const h = harness({}, { retryDelay: 10 }); t.after(() => h.store.dispose()); await h.ready();
  const lists = h.calls.threads; const connections = h.calls.connection; const reads = h.calls.snapshot.length;
  for (let revision = 2; revision < 200; revision++) h.streams.at(-1).emit('change', { threadId: 'a', revision, contentOnly: true });
  await wait(60);
  assert.ok(h.calls.snapshot.length > reads);
  assert.equal(h.calls.threads, lists);
  assert.equal(h.calls.connection, connections);
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(h.store.state.fresh, false);
});

test('all catalogue pages remain reachable beyond 128 sessions, including unknown or false capability', async (t) => {
  const entries = Array.from({ length: 177 }, (_, i) => ({ id: `id-${i}`, name: `Thread ${i}`, canAcceptDirectInput: i % 2 ? false : null }));
  const h = harness({ threads: async (query) => {
    const offset = Number(query.cursor || 0);
    assert.equal(query.limit, 50);
    assert.equal(query.archived, false);
    return { connected: true, threads: entries.slice(offset, offset + 50), nextCursor: offset + 50 < entries.length ? String(offset + 50) : undefined };
  } });
  t.after(() => h.store.dispose()); await h.ready();
  assert.equal(h.store.state.threads.length, 50);
  const reads = h.calls.snapshot.length;
  for (let page = 0; page < 3; page++) await h.store.loadMore();
  assert.equal(h.store.state.threads.length, 177);
  assert.equal(h.store.state.catalogue.nextCursor, undefined);
  assert.equal(h.calls.snapshot.length, reads, 'paging does not fetch transcripts');
  h.store.selectThread('id-176'); await wait(10);
  assert.equal(h.store.state.snapshot.thread.id, 'id-176');
  assert.equal(h.calls.mutations.length, 0);
});

test('late search results and cursors cannot overwrite a newer query or selected draft', async (t) => {
  const old = deferred(); const started = deferred(); let signal;
  const h = harness({ threads: async (query, readSignal) => {
    if (query.search === 'old') { signal = readSignal; started.resolve(); return old.promise; }
    return { connected: true, threads: [{ id: query.search || 'a', name: query.search || 'A' }], nextCursor: query.search ? 'new-cursor' : undefined };
  } });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('A draft');
  h.store.setSearch('old'); await started.promise;
  h.store.setSearch('new'); await wait(20);
  assert.equal(signal.aborted, true);
  assert.equal(h.store.state.threads[0].id, 'new', 'new query need not wait for a cancelled old fetch');
  old.resolve({ connected: true, threads: [{ id: 'stale', name: 'stale' }], nextCursor: 'wrong-cursor' }); await wait(10);
  assert.equal(h.store.state.threads[0].id, 'new');
  assert.equal(h.store.state.catalogue.nextCursor, 'new-cursor');
  assert.equal(h.store.state.selectedId, 'a');
  assert.equal(h.store.state.draft.text, 'A draft');
  assert.equal(h.store.state.controlsFresh, true);
});

test('changing archive filter fences a late page without pruning selection', async (t) => {
  const old = deferred(); let signal;
  const h = harness({ threads: async (query, readSignal) => {
    if (query.cursor) { signal = readSignal; return old.promise; }
    return { connected: true, threads: [{ id: query.archived ? 'archive' : 'a', archived: query.archived }], nextCursor: query.archived ? undefined : 'page-2' };
  } });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('still selected');
  const paging = h.store.loadMore(); await wait();
  h.store.setArchived(true); await wait(20);
  assert.equal(signal.aborted, true);
  assert.equal(h.store.state.threads[0].id, 'archive');
  old.resolve({ connected: true, threads: [{ id: 'late-nonarchived' }] }); await paging;
  assert.deepEqual(h.store.state.threads.map((thread) => thread.id), ['archive']);
  assert.equal(h.store.state.selectedId, 'a');
  assert.equal(h.store.state.selectedArchived, false);
  assert.equal(h.store.state.draft.text, 'still selected');
});

test('offpage selection retains draft and unknown gate across searches and metadata refresh', async (t) => {
  const h = harness({ mutate: async () => { throw new ApiError(0, 'network_error', true); } });
  t.after(() => h.store.dispose()); await h.ready();
  h.store.setDraft('do not replay A'); await h.store.send(); await h.store.recover();
  h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  h.data.list = { connected: true, threads: [{ id: 'b', name: 'B' }] };
  h.store.setSearch('B'); await wait(20);
  h.streams.at(-1).emit('change', { catalogueChanged: true }); await wait(20);
  assert.equal(h.store.state.selectedId, 'a');
  assert.equal(h.store.state.snapshot.thread.id, 'a');
  assert.equal(h.store.state.draft.text, 'do not replay A');
  assert.equal(h.store.state.gate.kind, 'unknown');
  await h.store.send(); assert.equal(h.calls.mutations.length, 1);
  h.store.selectThread('b'); await wait(10); h.store.setDraft('B draft');
  h.store.selectThread('a'); await wait(10);
  assert.equal(h.store.state.draft.text, 'do not replay A');
  assert.equal(h.store.state.gate.kind, 'unknown');
  await h.store.refresh();
  assert.equal(h.store.state.gate, undefined, 'deliberate full snapshot review works offpage');
  assert.equal(h.store.state.selectedId, 'a');
  assert.equal(h.calls.mutations.length, 1);
});

test('a blocked catalogue GET never blocks or cancels selected reads', async (t) => {
  const page = deferred(); const started = deferred(); let delay = false; let signal;
  const h = harness({ threads: (query, readSignal) => {
    if (delay) { signal = readSignal; started.resolve(); return page.promise; }
    return Promise.resolve({ connected: true, threads: [{ id: 'a' }, { id: 'b' }] });
  } });
  t.after(() => h.store.dispose()); await h.ready(); delay = true;
  const listing = h.store.refreshCatalogue(); await started.promise;
  h.store.selectThread('b'); await wait(20);
  assert.equal(h.store.state.snapshot.thread.id, 'b');
  assert.equal(h.store.state.controlsFresh, true);
  assert.equal(signal.aborted, false, 'selection does not own the list request');
  const reads = h.calls.snapshot.length;
  h.streams.at(-1).emit('change', { threadId: 'b', revision: 1, contentOnly: true }); await wait(20);
  assert.ok(h.calls.snapshot.length > reads, 'SSE-selected GET also proceeds while metadata waits');
  page.resolve({ connected: true, threads: [{ id: 'c' }] }); await listing;
  assert.equal(h.store.state.selectedId, 'b');
});

test('a blocked snapshot GET never blocks search or pagination, nor is aborted by them', async (t) => {
  const transcript = deferred(); const started = deferred(); let delay = false; let signal;
  const h = harness({
    snapshot: (id, readSignal) => { if (!delay) return Promise.resolve(snapshot(id)); signal = readSignal; started.resolve(); return transcript.promise; },
    threads: async (query) => ({ connected: true, threads: [{ id: query.cursor ? 'c' : query.search ? 'b' : 'a' }], nextCursor: query.cursor ? undefined : 'next' }),
  });
  t.after(() => h.store.dispose()); await h.ready(); delay = true;
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 2, contentOnly: true }); await started.promise;
  h.store.setSearch('B'); await wait(20);
  assert.equal(h.store.state.threads[0].id, 'b');
  await h.store.loadMore();
  assert.deepEqual(h.store.state.threads.map((thread) => thread.id), ['b', 'c']);
  assert.equal(signal.aborted, false);
  assert.equal(h.store.state.selectedId, 'a');
  transcript.resolve(snapshot('a', 2)); await wait(10);
  assert.equal(h.store.state.fresh, true);
});

test('a metadata error cannot disable healthy selected controls or replace its notice', async (t) => {
  let fail = false;
  const h = harness({ threads: async () => { if (fail) throw new ApiError(0, 'network_error'); return { connected: true, threads: [{ id: 'a' }] }; } });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('healthy');
  fail = true; await h.store.refreshCatalogue();
  assert.ok(h.store.state.catalogue.error);
  assert.equal(h.store.state.fresh, true);
  assert.equal(h.store.sendBlock(), '');
  assert.equal(h.store.state.notice, undefined);
});

test('live selected capability, not catalogue false or null, decides whether input is allowed', async (t) => {
  const h = harness(); t.after(() => h.store.dispose());
  h.data.list.threads[0].canAcceptDirectInput = false;
  await h.ready(); h.store.setDraft('allowed by live snapshot');
  assert.equal(h.store.sendBlock(), '');
  for (const capability of [false, null, undefined]) {
    h.data.snapshot = (id) => ({ ...snapshot(id), thread: { ...snapshot(id).thread, canAcceptDirectInput: capability } });
    await h.store.refresh();
    assert.match(h.store.mutationBlock(), /后端/);
    await h.store.send(); await h.store.interrupt();
    assert.equal(h.calls.mutations.length, 0);
  }
  h.data.list.threads[0].canAcceptDirectInput = null;
  h.data.snapshot = (id) => snapshot(id); await h.store.refresh();
  assert.equal(h.store.sendBlock(), '');
  h.data.snapshot = (id) => ({ ...snapshot(id), readOnly: true }); await h.store.refresh();
  assert.match(h.store.mutationBlock(), /只读/);
});

test('archived selection passes its read option even offpage and never auto-unarchives', async (t) => {
  const h = harness(); t.after(() => h.store.dispose()); await h.ready();
  h.data.list = { connected: true, threads: [{ id: 'archive', archived: true, canAcceptDirectInput: true }] };
  h.store.setArchived(true); await wait(20);
  h.store.selectThread('archive'); await wait(10);
  assert.equal(h.calls.reads.at(-1).archived, true);
  h.store.setDraft('never send'); await h.store.send();
  assert.match(h.store.mutationBlock(), /归档/);
  h.data.list = { connected: true, threads: [{ id: 'a' }] };
  h.store.setArchived(false); await wait(20); await h.store.refresh();
  assert.equal(h.store.state.selectedId, 'archive');
  assert.equal(h.calls.reads.at(-1).archived, true);
  assert.equal(h.calls.mutations.length, 0);
});

test('manual review discovering a new lifetime cannot unlock the old unknown gate', async (t) => {
  const h = harness({ mutate: async () => { throw new ApiError(0, 'network_error', true); } });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('once'); await h.store.send();
  await h.store.recover(); h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  h.data.connection = { ...h.data.connection, csrf: 'proof-2' };
  await h.store.refresh();
  assert.equal(h.store.state.gate.kind, 'unknown');
  h.streams.at(-1).emit('ready', { connected: true }); await wait(10);
  assert.equal(h.store.state.gate.kind, 'unknown');
  await h.store.refresh(); assert.equal(h.store.state.gate, undefined);
  assert.equal(h.calls.mutations.length, 1);
});

test('late connection and snapshot responses cannot resurrect a stopped lifetime', async (t) => {
  const connection = deferred(); const transcript = deferred(); const started = deferred();
  let connectionDelay = false; let readDelay = false;
  const h = harness({
    connection: () => connectionDelay ? connection.promise : Promise.resolve(h.data.connection),
    snapshot: (id) => { if (!readDelay) return Promise.resolve(snapshot(id)); started.resolve(); return transcript.promise; },
  });
  t.after(() => h.store.dispose()); await h.ready(); h.store.setDraft('retained');
  readDelay = true;
  h.streams.at(-1).emit('change', { threadId: 'a', revision: 2 }); await started.promise;
  connectionDelay = true; const oldCheck = h.store.refresh(); await wait();
  connectionDelay = false; h.data.connection = { sharing: false, connected: true };
  await h.store.recover();
  connection.resolve({ sharing: true, connected: true, csrf: 'proof-1' });
  transcript.resolve(snapshot('a', 200)); await oldCheck; await wait(10);
  assert.equal(h.store.state.link, 'remote-off');
  assert.equal(h.store.state.connection.csrf, undefined);
  assert.equal(h.store.state.snapshot, undefined);
  assert.equal(h.store.state.controlsFresh, false);
  assert.equal(h.store.state.draft.text, 'retained');
});

test('two browser stores sharing a service proof have independent selections, drafts and gates', async (t) => {
  const first = harness({ mutate: async () => { throw new ApiError(0, 'network_error', true); } });
  const second = harness(); t.after(() => { first.store.dispose(); second.store.dispose(); });
  await Promise.all([first.ready(), second.ready()]);
  first.store.setDraft('first tab A'); second.store.selectThread('b'); await wait(10); second.store.setDraft('second tab B');
  await first.store.send();
  second.store.setSearch('B'); await wait(20);
  assert.equal(first.store.state.selectedId, 'a');
  assert.equal(first.store.state.draft.text, 'first tab A');
  assert.equal(first.store.state.gate.kind, 'unknown');
  assert.equal(second.store.state.selectedId, 'b');
  assert.equal(second.store.state.draft.text, 'second tab B');
  assert.equal(second.store.state.gate, undefined);
  assert.equal(second.store.sendBlock(), '');
});
