import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { chromium } from 'playwright';

// Run only against the isolated synthetic preview, never a live Desktop bridge.
const base = process.env.MIRRORBIRD_PREVIEW_URL || 'http://localhost:43126';
const probe = await fetch(`${base}/__preview/state`);
assert.equal(probe.status, 200, 'Start tests/preview before running this test');
assert.equal((await probe.json()).synthetic, true, 'UI tests require the synthetic preview');
assert.equal((await fetch(`${base}/__preview/reset`, { method: 'POST' })).status, 204);
const browser = await chromium.launch({ channel: 'msedge', headless: true });
const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
const page = await context.newPage();
const errors = [];
page.on('pageerror', (error) => errors.push(error.message));
const artifacts = process.env.MIRRORBIRD_UI_ARTIFACTS;
if (artifacts) await mkdir(artifacts, { recursive: true });
const until = async (predicate, message, timeout = 15_000) => {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error(message);
};
const input = page.getByRole('textbox', { name: '消息输入', exact: true });
const send = page.locator('.send-button');
const ready = () => until(async () => (await page.locator('.connection-label').innerText()) === '已连接', 'Connection did not become live');
const idle = () => until(async () => await send.getAttribute('data-action') === 'send' && await input.inputValue() === '', 'Turn did not finish');
const submit = async (text) => {
  await input.fill(text);
  await until(() => send.isEnabled(), 'Send remained disabled');
  await send.click();
  await until(async () => await input.inputValue() === '', 'Server did not acknowledge message');
};
const select = async (name) => {
  if (await page.locator('.mobile-nav').isVisible()) await page.getByRole('button', { name: '打开会话列表', exact: true }).click();
  await page.locator('.session').filter({ hasText: name }).first().click();
  await until(async () => (await page.locator('.topbar-title').innerText()).includes(name), 'Selection did not change');
};
const screenshot = async (name) => { if (artifacts) await page.screenshot({ path: join(artifacts, name), fullPage: true }); };

try {
  await page.goto(base);
  await ready();
  await page.locator('[data-turn="demo-turn-1"]').waitFor();
  const focusStates = await page.evaluate(() => new Promise((resolve) => {
    const states = [], label = document.querySelector('.connection-label');
    const observer = new MutationObserver(() => states.push(label.textContent));
    observer.observe(label, { childList: true, characterData: true, subtree: true });
    window.dispatchEvent(new Event('focus'));
    document.dispatchEvent(new Event('visibilitychange'));
    setTimeout(() => { observer.disconnect(); resolve(states); }, 500);
  }));
  assert(focusStates.every((state) => state === '已连接'), `Focus reset a healthy connection: ${focusStates}`);
  await until(() => page.evaluate(() => navigator.serviceWorker.controller !== null), 'Offline shell did not install');
  assert.match(await page.locator('meta[name="viewport"]').getAttribute('content'), /maximum-scale=1.*user-scalable=no/);
  assert.equal(await page.locator('.archive-filter').count(), 0);
  assert(!(await page.locator('.sidebar').innerText()).includes('VS Code'));
  await page.getByRole('button', { name: '打开会话列表', exact: true }).click();
  assert.equal(await page.locator('.search-label input').evaluate((node) => parseFloat(getComputedStyle(node).fontSize)), 16);
  assert.equal(await page.locator('.search-label input').evaluate((node) => document.activeElement === node), false);
  assert.equal(await page.evaluate(() => visualViewport.scale), 1);
  await page.locator('.sidebar-close').click();
  assert.equal(await page.locator('.turn').count(), 8, 'Initial history must be bounded');
  assert(await page.locator('.conversation').evaluate((node) => node.scrollHeight - node.scrollTop - node.clientHeight < 80), 'Latest messages should be visible initially');
  const process = page.locator('[data-turn="demo-turn-1"] .turn-process');
  assert.equal(await process.getAttribute('open'), null);
  await process.locator(':scope > summary').click();
  assert.equal(await process.locator(':scope > summary').evaluate((node) => getComputedStyle(node).userSelect), 'none');
  await page.locator('.tool-group > summary').click();
  assert.match(await page.locator('.tool-group > summary').innerText(), /读取 1 个文件.*修改 1 个文件/);
  const command = page.locator('[data-disclosure="demo-command-1"] .tool-command');
  await command.click();
  assert.equal(await page.locator('[data-disclosure="demo-command-1"] .tool-output code').first().innerText(), await command.innerText(), 'Expanded command must retain the full text');
  await page.locator('[data-disclosure="demo-edit-1"] > summary').click();
  await screenshot('web-mobile.png');
  for (const width of [320, 360, 390, 768, 1280]) {
    await page.setViewportSize({ width, height: 844 });
    assert(await page.evaluate((width) => innerWidth === width && document.documentElement.scrollWidth <= width, width), `Page overflows at ${width}px`);
    for (const label of [command, page.locator('[data-disclosure="demo-edit-1"] .file-name')]) {
      assert(await label.evaluate((node) => {
        const style = getComputedStyle(node);
        return style.whiteSpace === 'nowrap' && style.textOverflow === 'ellipsis' && style.overflowX === 'hidden'
          && node.scrollWidth > node.clientWidth
          && node.clientHeight <= parseFloat(style.lineHeight) + parseFloat(style.paddingTop) + parseFloat(style.paddingBottom) + 1;
      }), `Long tool text was not truncated to one line at ${width}px`);
    }
  }
  await screenshot('web-desktop.png');
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator('.history-loader').click();
  await until(async () => await page.locator('.turn').count() > 8, 'Older history did not load');
  assert.notEqual(await process.getAttribute('open'), null, 'Loading history collapsed an existing disclosure');
  await select('等待工具审批');
  await page.locator('.approval').waitFor();
  await until(() => send.isEnabled(), 'Approval thread controls are not ready');
  assert.equal(await send.getAttribute('data-action'), 'stop');
  await submit('queued first');
  await submit('queued second');
  await until(async () => await page.locator('.pending-item:not(.submitting)').count() === 2, 'Messages did not reach server queue');
  const queued = page.locator('.pending-item:not(.submitting)');
  await until(() => queued.nth(0).getByRole('button', { name: '引导会话' }).isEnabled(), 'First queued message did not become available for guiding');
  assert(await queued.nth(1).getByRole('button', { name: '引导会话' }).isDisabled());
  await queued.nth(1).getByRole('button', { name: '撤回编辑' }).click();
  await until(async () => await input.inputValue() === 'queued second', 'Withdrawal did not restore draft');
  await input.fill('');
  await until(() => queued.first().getByRole('button', { name: '引导会话' }).isEnabled(), 'Queue guide did not become available');
  await queued.first().getByRole('button', { name: '引导会话' }).click();
  await until(async () => await queued.count() === 0 && await page.locator('.approval').count() === 0, 'Guide did not interrupt approval and start a new turn');
  await idle();
  const lastUser = page.locator('[data-role="user"]').last();
  await lastUser.dispatchEvent('pointerdown', { pointerType: 'touch', isPrimary: true, clientX: 100, clientY: 100 });
  await page.waitForTimeout(600);
  await lastUser.dispatchEvent('pointerup', { pointerType: 'touch', isPrimary: true, clientX: 100, clientY: 100 });
  await lastUser.dispatchEvent('click', { clientX: 100, clientY: 100, detail: 1 });
  assert.equal(await page.locator('.message-popover:popover-open').count(), 0, 'Long press must not open the app menu');
  await page.evaluate(() => {
    const bodies = [...document.querySelectorAll('[data-role] > [data-selection-boundary]')];
    const first = bodies.at(-2), last = bodies.at(-1);
    window.getSelection().setBaseAndExtent(first, 0, last, last.childNodes.length);
  });
  await until(() => page.evaluate(() => {
    const selection = getSelection(), range = selection.getRangeAt(0);
    const start = range.startContainer instanceof Element ? range.startContainer : range.startContainer.parentElement;
    const end = range.endContainer instanceof Element ? range.endContainer : range.endContainer.parentElement;
    return start.closest('[data-selection-boundary]') === end.closest('[data-selection-boundary]');
  }), 'Text selection crossed message boundaries');
  await page.evaluate(() => {
    const bodies = [...document.querySelectorAll('[data-role] > [data-selection-boundary]')];
    const first = bodies.at(-2), last = bodies.at(-1);
    last.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
    getSelection().setBaseAndExtent(last, last.childNodes.length, first, 0);
  });
  await until(() => page.evaluate(() => {
    const selection = getSelection(), range = selection.getRangeAt(0);
    const boundary = [...document.querySelectorAll('[data-role] > [data-selection-boundary]')].at(-1);
    return boundary?.contains(range.startContainer) && boundary.contains(range.endContainer) && selection.direction === 'backward';
  }), 'Reverse selection was not contained in its original message');
  await page.evaluate(() => {
    document.querySelector('[data-selection-boundary]').dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
    getSelection().selectAllChildren(document.body);
  });
  await until(() => page.evaluate(() => {
    const range = getSelection().getRangeAt(0);
    const start = range.startContainer instanceof Element ? range.startContainer : range.startContainer.parentElement;
    const end = range.endContainer instanceof Element ? range.endContainer : range.endContainer.parentElement;
    return start.closest('[data-selection-boundary]') !== null && start.closest('[data-selection-boundary]') === end.closest('[data-selection-boundary]');
  }), 'Select all crossed message boundaries');
  await page.evaluate(() => getSelection().removeAllRanges());
  await lastUser.locator('.prose').click();
  await page.getByRole('menuitem', { name: '编辑消息' }).click();
  await page.getByRole('textbox', { name: '消息内容', exact: true }).fill('edited message');
  await page.getByRole('button', { name: '保存并重新生成', exact: true }).click();
  await page.getByRole('dialog', { name: '编辑消息' }).waitFor({ state: 'hidden' });
  await until(async () => (await page.locator('[data-role="user"]').last().innerText()).includes('edited message'), 'Edit did not replace the user turn');
  await idle();
  await page.locator('[data-role="assistant"]').last().locator('.prose').click();
  await page.getByRole('menuitem', { name: '分支到新聊天' }).click();
  await until(async () => (await page.locator('.topbar-title').innerText()).includes('分支'), 'Fork did not open the new thread');
  await page.getByRole('button', { name: '更多会话操作', exact: true }).click();
  await page.locator('[data-option="rename"]').click();
  await page.getByRole('textbox', { name: '会话名称', exact: true }).fill('UI verified fork');
  await page.getByRole('button', { name: '保存', exact: true }).click();
  await until(async () => await page.locator('.topbar-title').innerText() === 'UI verified fork', 'Rename did not synchronize');
  await page.locator('.composer-choice').first().click();
  await page.locator('[data-option="demo-model"]').click();
  await until(async () => (await page.locator('.composer-choice').first().innerText()).includes('演示模型'), 'Model selection did not persist');
  await until(() => page.locator('.composer-choice').nth(1).isEnabled(), 'Settings have not synchronized');
  await page.locator('.composer-choice').nth(1).click();
  await page.locator('[data-option="high"]').click();
  await until(async () => (await page.locator('.composer-choice').nth(1).innerText()).includes('高'), 'Reasoning selection did not persist');
  await until(() => page.locator('.composer-add').isEnabled(), 'Composer is not ready');
  await page.locator('.composer-add').click();
  await page.locator('[data-option="demo-review"]').click();
  assert.match(await input.inputValue(), /code-review/);
  await page.locator('.skill-chip').click();
  await input.fill('');
  await page.locator('.composer-add').click();
  await page.locator('[data-option="compact"]').click();
  await until(async () => await page.locator('.timeline-event').count() > 0, 'Compaction did not synchronize');
  let initializationRequests = 0, splitOptionsRequests = 0, releaseInitialization;
  const initializationGate = new Promise((resolve) => { releaseInitialization = resolve; });
  const initialize = async (route) => {
    initializationRequests++;
    await initializationGate;
    await route.continue();
  };
  const trackOptions = (request) => {
    if (new URL(request.url()).pathname === '/api/options') splitOptionsRequests++;
  };
  page.on('request', trackOptions);
  await page.route('**/api/new-thread', initialize);
  await context.setOffline(true);
  await until(async () => (await page.locator('.connection-label').innerText()) !== '已连接', 'Offline state was not detected before new conversation');
  await page.getByRole('button', { name: '打开会话列表', exact: true }).click();
  await page.getByRole('button', { name: '新建会话', exact: true }).click();
  assert.match(await page.locator('#projectSelect').innerText(), /等待 Desktop 连接/);
  assert.equal(initializationRequests, 0, 'Initialization should wait for a confirmed connection');
  await input.fill('Created by UI test');
  assert(await send.isDisabled(), 'Disconnected new conversation cannot send');
  await context.setOffline(false);
  await until(() => initializationRequests === 1, 'New conversation options did not load when connection recovered');
  assert.match(await page.locator('#projectSelect').innerText(), /正在加载项目/);
  assert(await send.isDisabled(), 'Initialization must finish before creation');
  releaseInitialization();
  await ready();
  await until(async () => await page.locator('#projectSelect option').count() === 2, 'Projects did not load');
  assert.equal(await input.inputValue(), 'Created by UI test', 'Initialization must retain the draft');
  assert.match(await page.locator('.composer-choice').first().innerText(), /演示模型/, 'Default model must be visible before opening the picker');
  assert.equal(await page.locator('.composer-choice').nth(1).innerText(), '中等', 'Default reasoning effort must be visible before opening the picker');
  await page.locator('.composer-choice').first().click();
  await page.locator('[data-option="demo-model"]').waitFor();
  await page.keyboard.press('Escape');
  await page.locator('.composer-choice').nth(1).click();
  await page.locator('[data-option="high"]').click();
  assert.equal(initializationRequests, 1, 'Pickers should reuse the combined initialization response');
  assert.equal(splitOptionsRequests, 0, 'New conversation must not fetch projects/models separately');
  await page.locator('#projectSelect').selectOption('other-project');
  await context.setOffline(true);
  await until(async () => (await page.locator('.connection-label').innerText()) !== '已连接', 'New conversation disconnect was not detected');
  await context.setOffline(false);
  await ready();
  await until(async () => initializationRequests === 2 && await page.locator('#projectSelect').isEnabled(), 'New conversation configuration did not recover');
  assert.equal(await page.locator('#projectSelect').inputValue(), 'other-project', 'Reconnect must retain the chosen project');
  assert.equal(await page.locator('.composer-choice').nth(1).innerText(), '高', 'Reconnect must retain the chosen reasoning effort');
  assert.equal(await input.inputValue(), 'Created by UI test', 'Reconnect must retain the new conversation draft');
  assert.equal(splitOptionsRequests, 0);
  await screenshot('web-new-conversation.png');
  await page.unroute('**/api/new-thread', initialize);
  page.off('request', trackOptions);
  await submit('Created by UI test');
  await until(async () => (await page.locator('.topbar-title').innerText()).includes('Created by UI test'), 'Creation did not open the new project thread');
  await until(async () => await send.getAttribute('data-action') === 'stop' && await send.isEnabled(), 'New turn did not start');
  await send.click();
  await page.getByRole('dialog', { name: '停止当前任务？' }).getByRole('button', { name: '停止任务', exact: true }).click();
  await idle();
  await input.fill('offline draft');
  await context.setOffline(true);
  await until(async () => (await page.locator('.connection-label').innerText()) !== '已连接', 'Offline state was not detected');
  await page.reload();
  await input.waitFor();
  await until(async () => await input.inputValue() === 'offline draft', 'Offline draft was not restored');
  assert.match(await page.locator('.topbar-title').innerText(), /Created by UI test/);
  assert(await send.isDisabled(), 'Cached content must not authorize a send');
  assert(await page.locator('[data-role="user"]').count() > 0, 'Offline conversation was not restored');
  await screenshot('web-offline.png');
  await context.setOffline(false);
  await ready();
  await until(() => send.isEnabled(), 'Online recovery did not restore controls');
  assert.equal(await input.inputValue(), 'offline draft', 'Reconnect must not auto-send drafts');
  assert.deepEqual(errors, [], 'Browser runtime errors');
  console.log('UI passed: responsive layout, history, disclosure, queue/guide/withdraw, edit, fork, rename, settings, skills, compact, combined new conversation initialization/defaults/recovery, create, stop, PWA offline/recovery.');
} catch (error) {
  console.error((await page.locator('body').innerText()).slice(-4000));
  await screenshot('web-failure.png');
  throw error;
} finally {
  await browser.close();
}
