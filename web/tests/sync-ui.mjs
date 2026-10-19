import assert from 'node:assert/strict';
import { chromium } from 'playwright';

const base = process.env.MIRRORBIRD_PREVIEW_URL || 'http://localhost:43126';
assert.equal((await (await fetch(`${base}/__preview/state`)).json()).synthetic, true);
const step = async (action) => assert.equal((await fetch(`${base}/__preview/incremental/${action}`, { method: 'POST' })).status, 204);
assert.equal((await fetch(`${base}/__preview/reset`, { method: 'POST' })).status, 204);
await step('prepare');
const browser = await chromium.launch({ channel: 'msedge', headless: true });
const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
const page = await context.newPage();
const errors = [], responses = [], reads = [];
let stable = false;
page.on('pageerror', (error) => errors.push(error.message));
page.on('response', (response) => {
  if (!new URL(response.url()).pathname.endsWith('/sync')) return;
  const required = stable;
  reads.push(response.body().then((body) => {
    responses.push({ bytes: body.byteLength, ...JSON.parse(body.toString()) });
  }).catch((error) => { if (required) errors.push(error.message); }));
});
const until = async (predicate) => {
  const deadline = Date.now() + 15_000;
  while (!await predicate()) {
    if (Date.now() > deadline) throw new Error('Timed out waiting for incremental DOM update');
    await new Promise((resolve) => setTimeout(resolve, 30));
  }
};

try {
  await page.goto(base);
  await page.locator('[data-item="perf-answer"]').waitFor();
  await until(async () => await page.locator('.connection-label').innerText() === '已连接');
  await Promise.all(reads);
  const initial = responses.find((response) => response.mode === 'snapshot');
  assert(initial.bytes > 1_000_000, `Fixture must exceed 1 MB: ${initial.bytes}`);
  responses.length = reads.length = 0;
  stable = true;
  await page.evaluate(() => {
    const user = document.querySelector('[data-item="perf-user"]');
    const answer = document.querySelector('[data-item="perf-answer"] .prose p').firstChild;
    const output = document.querySelectorAll('[data-item="perf-running"] code')[1].firstChild;
    const finished = document.querySelector('[data-item="perf-finished"]');
    const old = document.querySelector('[data-turn="perf-old-1"]');
    const tool = document.querySelector('[data-item="perf-running"]');
    tool.open = true;
    let mutations = 0;
    const observer = new MutationObserver((records) => { mutations += records.length; });
    observer.observe(old, { subtree: true, childList: true, characterData: true, attributes: true });
    const range = document.createRange();
    range.selectNodeContents(user.querySelector('.prose'));
    getSelection().removeAllRanges(); getSelection().addRange(range);
    window.syncCheck = { user, answer, output, finished, old, tool, observer, mutations: () => mutations, selection: getSelection().toString() };
  });
  for (let i = 1; i <= 12; i++) {
    await step('append');
    await until(async () => (await page.locator('[data-item="perf-answer"] .prose').innerText()).match(/增量/g)?.length === i);
  }
  const identities = await page.evaluate(() => {
    const check = window.syncCheck;
    return {
      user: check.user === document.querySelector('[data-item="perf-user"]'),
      answerText: check.answer === document.querySelector('[data-item="perf-answer"] .prose p').firstChild,
      outputText: check.output === document.querySelectorAll('[data-item="perf-running"] code')[1].firstChild,
      finished: check.finished === document.querySelector('[data-item="perf-finished"]'),
      old: check.old === document.querySelector('[data-turn="perf-old-1"]'),
      expanded: check.tool.open,
      selection: check.selection === getSelection().toString(),
      mutations: check.mutations(),
    };
  });
  assert.deepEqual(identities, { user: true, answerText: true, outputText: true, finished: true, old: true, expanded: true, selection: true, mutations: 0 });
  await step('finish');
  await until(async () => await page.locator('[data-turn="perf-live"] .turn-process').count() === 1);
  await until(() => page.locator('.composer-add').isEnabled());
  let release, arrived;
  const held = new Promise((resolve) => { release = resolve; });
  const requested = new Promise((resolve) => { arrived = resolve; });
  const holdSync = async (route) => { arrived(); await held; await route.continue(); };
  await page.route('**/api/threads/demo-thread/sync?*', holdSync);
  await step('finish');
  await requested;
  await page.locator('[data-item="perf-answer"] .prose').click();
  const fork = page.getByRole('menuitem', { name: '分支到新聊天' });
  assert(await fork.isDisabled(), 'Open actions must respect an outstanding structural update');
  release();
  await until(() => fork.isEnabled());
  await page.keyboard.press('Escape');
  await page.unroute('**/api/threads/demo-thread/sync?*', holdSync);
  await step('trim');
  await page.locator('[data-turn="perf-next"]').waitFor();
  assert.equal(await page.locator('[data-turn="perf-old-0"]').count(), 0);
  assert(await page.evaluate(() => window.syncCheck.old === document.querySelector('[data-turn="perf-old-1"]')));
  await Promise.all(reads);
  assert(responses.length >= 12);
  assert(responses.every((response) => response.mode === 'delta'), 'A healthy connection fetched another full snapshot');
  const bytes = responses.reduce((total, response) => total + response.bytes, 0);
  assert(bytes < 20_000, `Incremental transfer exceeded budget: ${bytes}`);
  assert.deepEqual(errors, []);
  console.log(`Incremental UI: initial ${initial.bytes} bytes; ${responses.length} updates ${bytes} bytes; unchanged DOM/selection preserved.`);
} finally {
  await context.close();
  await browser.close();
}
