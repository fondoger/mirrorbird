import assert from 'node:assert/strict';
import { chromium } from 'playwright';

const base = process.env.MIRRORBIRD_PREVIEW_URL || 'http://localhost:43126';
const probe = await fetch(`${base}/__preview/state`);
assert.equal(probe.status, 200, 'Start tests/preview before running this test');
assert.equal((await probe.json()).synthetic, true, 'UI tests require the synthetic preview');
const browser = await chromium.launch({ channel: 'msedge', headless: true });
const page = await browser.newPage({ viewport: { width: 390, height: 844 }, serviceWorkers: 'block' });
const errors = [];
page.on('pageerror', (error) => errors.push(error.message));
const items = [
  { id: 'user', type: 'userMessage', content: [{ type: 'text', text: 'Inspect a long history' }] },
  ...Array.from({ length: 1084 }, (_, i) => ({ id: `tool-${i}`, type: 'commandExecution', command: `command-${i}`,
    status: 'completed', aggregatedOutput: `complete-output-${i}\n${'x'.repeat(4200)}` })),
  { id: 'final', type: 'agentMessage', phase: 'final_answer', text: 'The complete final answer' },
];
const thread = { id: 'demo-thread', name: 'Large history', cwd: '', status: { type: 'idle' }, canAcceptDirectInput: true,
  turns: [{ id: 'large', status: 'completed', itemsView: 'summary', items: [items[0], items.at(-1)] }] };
const cursors = [];
let failOnce = true, posts = 0;
const until = async (predicate, message) => {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error(message);
};
try {
  await page.route('**/api/**', async (route) => {
    const request = route.request(), url = new URL(request.url());
    if (request.method() !== 'GET') { posts++; return route.abort(); }
    const reply = (json, status = 200) => route.fulfill({ json, status });
    if (url.pathname === '/api/connection') return reply({ sharing: true, connected: true, csrf: 'synthetic-proof' });
    if (url.pathname === '/api/threads') return reply({ connected: true, threads: [{ ...thread, status: 'idle', turns: undefined }] });
    if (url.pathname === '/api/threads/demo-thread') return reply({ thread, revision: 1, historyVersion: 7, pending: [] });
    if (url.pathname.endsWith('/turns/large/items')) {
      const start = Number(url.searchParams.get('cursor') || 0);
      assert.equal(url.searchParams.get('version'), '7');
      cursors.push(start);
      if (start === 16 && failOnce) { failOnce = false; return reply({ code: 'operation_rejected' }, 502); }
      const end = Math.min(start + 16, items.length);
      return reply({ turnId: 'large', items: items.slice(start, end), nextCursor: end < items.length ? String(end) : undefined, historyVersion: 7 });
    }
    return route.continue();
  });
  await page.goto(base);
  await until(async () => await page.locator('.connection-label').innerText() === '已连接', 'Connection did not become live');
  assert.equal(cursors.length, 0, 'Opening a thread must not eagerly load tool history');
  assert.equal(await page.locator('[data-item="user"]').count(), 1);
  assert.equal(await page.locator('[data-item="final"]').count(), 1);
  const process = page.locator('[data-turn="large"] .turn-process');
  await process.locator(':scope > summary').click();
  await until(async () => /点击重试/.test(await page.locator('[data-load-items]').innerText()), 'Page failure was not visible');
  assert.equal(await page.locator('.tool-details').count(), 15, 'Successful first page must survive the failure');
  await page.locator('.tool-group > summary').click();
  await page.locator('[data-load-items]').click();
  await until(async () => await page.locator('[data-load-items]').count() === 0, 'Full history did not finish paging');
  assert.deepEqual(cursors, [0, 16, ...Array.from({ length: Math.ceil(items.length / 16) - 1 }, (_, i) => (i + 1) * 16)]);
  assert.equal(await page.locator('.tool-details').count(), 1084);
  assert.equal(await page.locator('[data-item="user"]').count(), 1);
  assert.equal(await page.locator('[data-item="final"]').count(), 1);
  assert.notEqual(await page.locator('.tool-group').getAttribute('open'), null, 'Paging collapsed the open tool group');
  await page.locator('[data-item="tool-1083"] > summary').click();
  assert.equal(await page.locator('[data-item="tool-1083"] .tool-output code').last().innerText(), items.at(-2).aggregatedOutput);
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'Details overflow the mobile viewport');
  assert.equal(posts, 0, 'History hydration must not execute operations');
  assert.deepEqual(errors, []);
  console.log('Large-history UI: all 1086 records preserved; retry, disclosure state and mobile layout passed.');
} finally {
  await browser.close();
}
