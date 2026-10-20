import test from 'node:test';
import assert from 'node:assert/strict';
import { HttpApi, ApiError, friendlyError } from '../src/transport.ts';
import { parseConnection, parseSnapshot, parseThreadList, sourceName } from '../src/types.ts';
import { approvalContext, inputText, detailedTurn } from '../src/transcript.ts';

const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const mutation = { action: 'messages', text: '继续检查', mode: 'start' };

const newThreadOptions = {
  projects: [{ id: 'p', name: 'Project', roots: [{ path: 'C:\\repo' }] }],
  models: [
    { id: 'catalogue', model: 'catalogue', isDefault: true, defaultReasoningEffort: 'low', supportedReasoningEfforts: [{ reasoningEffort: 'low' }] },
    { id: 'configured', model: 'configured', displayName: 'Configured model', defaultReasoningEffort: 'medium', supportedReasoningEfforts: [{ reasoningEffort: 'medium' }, { reasoningEffort: 'high' }] },
  ],
  defaults: { model: 'configured', effort: 'high' },
};

test('new conversation loads projects, models and effective defaults in one request', async () => {
  let calls = 0;
  const api = new HttpApi(async (path, init) => {
    calls++;
    assert.equal(path, '/api/new-thread');
    assert.equal(init.method, 'GET');
    assert.equal(init.cache, 'no-store');
    assert.equal(init.credentials, 'same-origin');
    assert.equal(init.body, undefined);
    return json(newThreadOptions);
  });
  const options = await api.newThreadOptions();
  assert.equal(calls, 1);
  assert.equal(options.projects[0].id, 'p');
  assert.equal(options.models.length, 2);
  assert.deepEqual(options.defaults, { model: 'configured', effort: 'high' });
});

test('new conversation rejects incomplete or mismatched initialization instead of showing an empty project list', async () => {
  for (const payload of [
    {}, { ...newThreadOptions, projects: null }, { ...newThreadOptions, models: undefined },
    { ...newThreadOptions, defaults: {} },
    { ...newThreadOptions, defaults: { model: 'missing', effort: 'high' } },
    { ...newThreadOptions, defaults: { model: 'configured', effort: 'invalid' } },
  ]) {
    const api = new HttpApi(async () => json(payload));
    await assert.rejects(api.newThreadOptions(), (error) => error instanceof ApiError && error.code === 'invalid_response' && !error.unknownOutcome);
  }
  const api = new HttpApi(async () => json({ projects: [], models: [], defaults: { model: '', effort: '' } }));
  assert.deepEqual(await api.newThreadOptions(), { projects: [], models: [], defaults: { model: '', effort: '' } });
});

test('leaving new conversation cancels initialization without a retry or mutation', async () => {
  const controller = new AbortController();
  let calls = 0;
  const api = new HttpApi((_path, init) => {
    calls++;
    return new Promise((_resolve, reject) => init.signal.addEventListener('abort', () => reject(new DOMException('Cancelled', 'AbortError')), { once: true }));
  });
  const pending = api.newThreadOptions(controller.signal);
  controller.abort();
  await assert.rejects(pending, (error) => error.name === 'AbortError');
  assert.equal(calls, 1);
});

test('item history encodes identifiers and retains complete tool results', async () => {
  const api = new HttpApi(async (path, init) => {
    const url = new URL(path, 'http://localhost');
    assert.equal(url.pathname, '/api/threads/a%2Fb/turns/t%2Fx/items');
    assert.equal(url.searchParams.get('cursor'), 'opaque+/==');
    assert.equal(url.searchParams.get('version'), '42');
    assert.equal(init.method, 'GET');
    return json({ turnId: 't/x', historyVersion: 42, nextCursor: 'more', items: [
      { id: 'tool', type: 'commandExecution', aggregatedOutput: 'complete output', command: 'test' },
    ] });
  });
  const page = await api.items('a/b', 't/x', 'opaque+/==', 42);
  assert.equal(page.items[0].aggregatedOutput, 'complete output');
  assert.equal(page.nextCursor, 'more');
});

test('partial details preserve summary messages without duplicate final replies', () => {
  const user = { id: 'user', type: 'userMessage' }, final = { id: 'final', type: 'agentMessage' };
  const turn = { id: 't', status: 'completed', itemsView: 'summary', items: [user, final] };
  const tool = { id: 'tool', type: 'commandExecution' };
  assert.deepEqual(detailedTurn(turn, { loaded: true, items: [user, tool] }).items, [user, tool, final]);
  assert.deepEqual(detailedTurn(turn, { loaded: true, items: [user, tool, final] }).items, [user, tool, final]);
  assert.equal(detailedTurn(turn), turn);
});

test('companion create, queue and actions keep exact payloads and deliberate request IDs', async () => {
  const calls = [];
  const api = new HttpApi(async (path, init) => {
    calls.push({ path, body: JSON.parse(init.body) });
    return json({ accepted: true, threadId: 'created' }, 202);
  });
  assert.deepEqual(await api.mutate('@new', 'proof', { action: 'create', projectId: 'p', text: 'new' }, 'r1'), { threadId: 'created' });
  await api.mutate('a', 'proof', { action: 'queue', text: 'later', skillId: 'installed', model: 'm', effort: 'high' }, 'r2');
  await api.mutate('a', 'proof', { action: 'actions', kind: 'fork', turnId: 'turn' }, 'r3');
  assert.deepEqual(calls, [
    { path: '/api/threads', body: { projectId: 'p', text: 'new', requestId: 'r1' } },
    { path: '/api/threads/a/queue', body: { text: 'later', skillId: 'installed', model: 'm', effort: 'high', requestId: 'r2' } },
    { path: '/api/threads/a/actions', body: { kind: 'fork', turnId: 'turn', requestId: 'r3' } },
  ]);
});

test('unsupported companion operations are explicit rejections, not unknown outcomes', async () => {
  for (const code of ['unsupported', 'attachment_outdated']) {
    const api = new HttpApi(async () => json({ code }, 501));
    await assert.rejects(api.mutate('a', 'proof', { action: 'actions', kind: 'compact' }, 'r1'),
      (error) => error.code === code && !error.unknownOutcome);
  }
  assert.match(friendlyError(new ApiError(501, 'attachment_outdated')), /完全退出并重新打开 ChatGPT Desktop/);
});

test('protocol provenance is not presented as support for a different desktop product', () => {
  for (const source of ['vscode', 'appServer', undefined]) assert.equal(sourceName(source), 'Codex');
});

test('installed skill input is named without exposing its local path', () => {
  assert.equal(inputText([{ type: 'skill', name: 'review', path: 'C:\\private\\SKILL.md' }]), 'Skill · review');
});

test('a dropped mutation response is unknown and POST is attempted exactly once', async () => {
  let calls = 0;
  const api = new HttpApi(async () => { calls++; throw new TypeError('Network lost'); });
  await assert.rejects(api.mutate('thread', 'csrf', mutation, 'unique-request-id'), (error) => error instanceof ApiError && error.unknownOutcome);
  assert.equal(calls, 1);
});

test('mutation uses same-origin JSON, CSRF proof, opaque IDs and one deliberate request ID', async () => {
  let captured;
  const api = new HttpApi(async (path, init) => { captured = { path, init }; return json({ accepted: true }, 202); });
  const handle = 'opaque__900719925474099312345';
  await api.mutate('thread/with space', 'secret-proof', { action: 'respond', id: handle, decision: 'accept' }, 'random-request-id');
  assert.equal(captured.path, '/api/threads/thread%2Fwith%20space/respond');
  assert.equal(captured.init.credentials, 'same-origin');
  assert.equal(captured.init.cache, 'no-store');
  assert.equal(captured.init.redirect, 'error');
  assert.equal(captured.init.headers['X-MirrorBird-CSRF'], 'secret-proof');
  assert.deepEqual(JSON.parse(captured.init.body), { id: handle, decision: 'accept', requestId: 'random-request-id' });
});

test('network, malformed success, unknown operation, and unclassified server errors never retry', async () => {
  for (const response of [
    () => json({ accepted: false }, 202),
    () => new Response('<html>signed out</html>', { status: 200, headers: { 'Content-Type': 'text/html' } }),
    () => json({ error: 'sensitive upstream output', code: 'operation_failed_or_outcome_unknown' }, 409),
    () => json({ error: 'sensitive upstream output', code: 'outcome_unknown' }, 502),
    () => json({ error: 'sensitive upstream output' }, 502),
  ]) {
    let calls = 0;
    const api = new HttpApi(async () => { calls++; return response(); });
    await assert.rejects(api.mutate('t', 'proof', mutation, 'request-id'), (error) => {
      assert.ok(!error.message.includes('sensitive'));
      return error.unknownOutcome === true;
    });
    assert.equal(calls, 1);
  }
});

test('stable pre-admission rejection is distinguished from unknown outcome', async () => {
  for (const code of ['state_changed', 'resync_required', 'operation_rejected', 'read_only', 'disconnected', 'capacity_limited', 'invalid_input', 'sharing_disabled', 'csrf_invalid']) {
    const api = new HttpApi(async () => json({ code, error: 'private text' }, code === 'disconnected' ? 503 : 409));
    await assert.rejects(api.mutate('t', 'proof', mutation, 'request-id'), (error) => !error.unknownOutcome && error.code === code);
  }
});

test('connection boots without a MirrorBird cookie, while retaining relay credentials', async () => {
  let calls = 0;
  const api = new HttpApi(async (path, init) => {
    calls++;
    assert.equal(path, '/api/connection');
    assert.equal(init.method, 'GET');
    assert.equal(init.headers.Cookie, undefined);
    assert.equal(init.headers['X-MirrorBird-CSRF'], undefined);
    assert.equal(init.credentials, 'same-origin');
    assert.equal(init.body, undefined);
    return json({ sharing: true, connected: true, csrf: 'lifetime-proof' });
  });
  assert.deepEqual(await api.connection(), { sharing: true, connected: true, csrf: 'lifetime-proof' });
  assert.equal(calls, 1);
  assert.throws(() => parseConnection({ sharing: true, connected: true }), /proof/);
  assert.deepEqual(parseConnection({ sharing: false, connected: false }), { sharing: false, connected: false, csrf: undefined });
});

test('catalogue encodes server search and cursor without a visibility cap', async () => {
  const entries = Array.from({ length: 160 }, (_, i) => ({ id: `t-${i}`, name: `thread ${i}`, canAcceptDirectInput: i % 2 ? false : null }));
  const parsed = parseThreadList({ threads: entries, nextCursor: 'next page', connected: true });
  assert.equal(parsed.threads.length, 160);
  assert.equal(parsed.threads[1].canAcceptDirectInput, false);
  assert.equal(parsed.threads[0].canAcceptDirectInput, null);
  const api = new HttpApi(async (path, init) => {
    const url = new URL(path, 'http://localhost');
    assert.equal(url.pathname, '/api/threads');
    assert.equal(url.searchParams.get('search'), '名 & project');
    assert.equal(url.searchParams.get('cursor'), 'opaque+next/==');
    assert.equal(url.searchParams.has('archived'), false);
    assert.equal(url.searchParams.get('limit'), '50');
    assert.equal(init.method, 'GET');
    return json({ threads: entries, nextCursor: 'next page', connected: true });
  });
  const page = await api.threads({ search: '名 & project', cursor: 'opaque+next/==' });
  assert.equal(page.threads.length, 160);
  assert.equal(page.nextCursor, 'next page');
});

test('read-only snapshots and deliberate resync use GET without an archive browsing option', async () => {
  const paths = [];
  const api = new HttpApi(async (path, init) => {
    paths.push(path); assert.equal(init.method, 'GET'); assert.equal(init.body, undefined);
    return json({ thread: { id: 'thread/id', canAcceptDirectInput: null }, readOnly: true, revision: 1 });
  });
  const view = await api.snapshot('thread/id');
  assert.equal(view.readOnly, true);
  assert.equal(view.thread.canAcceptDirectInput, null);
  await api.snapshot('thread/id', { resync: true });
  assert.deepEqual(paths, ['/api/threads/thread%2Fid', '/api/threads/thread%2Fid?resync=true']);
});

test('lazy snapshot reads receive 50 seconds while connection and catalogue retain 20 seconds', async (t) => {
  const delays = [];
  const original = globalThis.setTimeout;
  t.mock.method(globalThis, 'setTimeout', (callback, delay, ...args) => { delays.push(delay); return original(callback, delay, ...args); });
  const api = new HttpApi(async (path) => path === '/api/connection'
    ? json({ sharing: true, connected: true, csrf: 'proof' })
    : path.startsWith('/api/threads?') ? json({ connected: true, threads: [] })
      : json({ thread: { id: 'a' }, revision: 1 }));
  await api.connection(); await api.threads(); await api.snapshot('a');
  assert.deepEqual(delays, [20_000, 20_000, 50_000]);
});

test('snapshot parser refuses numeric approval IDs rather than rounding them', () => {
  const snapshot = { thread: { id: 't', name: '名称', cwd: '', status: { type: 'active' }, turns: null }, revision: 1, pending: [{ id: 9007199254740992, method: 'x', params: {} }] };
  assert.throws(() => parseSnapshot(snapshot), /identifier/);
  snapshot.pending[0].id = 'opaque-string';
  assert.equal(parseSnapshot(snapshot).pending[0].id, 'opaque-string');
});

test('unsupported or incomplete approvals cannot be accepted; complete context is not silently truncated', () => {
  const command = 'printf "<script>untrusted</script>"';
  const request = { id: 'opaque', method: 'item/commandExecution/requestApproval', params: { command, cwd: 'C:/repo' } };
  assert.ok(approvalContext(request).canRespond);
  assert.ok(approvalContext(request).context.includes(command));
  assert.equal(approvalContext({ ...request, method: 'item/tool/requestUserInput' }).canRespond, false);
  assert.equal(approvalContext({ ...request, desktopOnly: true }).canRespond, false);
  assert.equal(approvalContext({ ...request, params: { command: 'echo ' + String.fromCharCode(8238) + 'unsafe' } }).canRespond, false);
  assert.equal(approvalContext({ ...request, params: { threadId: 't' } }).canRespond, false);
  assert.equal(approvalContext({ ...request, params: { cwd: 'C:/repo', reason: 'Permission needed' } }).canRespond, false);
  assert.equal(approvalContext({ ...request, method: 'item/fileChange/requestApproval', params: { cwd: 'C:/repo' } }).canRespond, false);
  assert.equal(approvalContext({ ...request, method: 'item/fileChange/requestApproval', params: { grantRoot: 'C:/repo' } }).canRespond, true);
  assert.equal(approvalContext({ ...request, params: { command: 'x'.repeat(70_000) } }).canRespond, false);
});

test('message content only exposes text or explicit Desktop placeholders, never image URLs', () => {
  assert.equal(inputText([{ type: 'text', text: 'safe text' }, { type: 'image', url: 'https://remote.invalid/tracker' }]), 'safe text\n［图片，请在 Desktop 查看］');
});
