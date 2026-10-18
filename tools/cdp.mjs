import fs from 'node:fs';

const [command = 'targets', ...args] = process.argv.slice(2);
const origin = process.env.MIRRORBIRD_CDP_URL ?? 'http://127.0.0.1:9229';
const url = new URL(origin);
if (!['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname)) throw new Error('Desktop debugging is loopback-only');
const targets = await (await fetch(new URL('/json/list', origin))).json();
if (command === 'targets') {
  console.log(JSON.stringify(targets.map(({ id, type, url }) => ({ id, type, url })), null, 2));
  process.exit(0);
}
const requestedTarget = process.env.MIRRORBIRD_CDP_TARGET;
const page = requestedTarget
  ? targets.find(t => t.id === requestedTarget)
  : targets.find(t => t.type === 'page' && !t.url.startsWith('devtools://'));
if (!page?.webSocketDebuggerUrl) throw new Error(requestedTarget ? 'Requested Desktop page no longer exists; refusing to select another tab' : 'No Desktop page found');
const socket = new WebSocket(page.webSocketDebuggerUrl);
await new Promise((resolve, reject) => { socket.addEventListener('open', resolve, { once: true }); socket.addEventListener('error', reject, { once: true }); });
let next = 0;
const pending = new Map();
socket.addEventListener('message', event => {
  const data = JSON.parse(event.data);
  if (data.id && pending.has(data.id)) {
    const { resolve, reject, timer } = pending.get(data.id);
    pending.delete(data.id); clearTimeout(timer);
    if (data.error) reject(new Error(data.error.message)); else resolve(data.result);
  }
});
function call(method, params = {}) {
  const id = ++next;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`${method} timed out`)); }, 15000);
    pending.set(id, { resolve, reject, timer });
    socket.send(JSON.stringify({ id, method, params }));
  });
}
async function evaluate(expression) {
  const result = await call('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  if (result.exceptionDetails) throw new Error(result.exceptionDetails.text);
  return result.result?.value;
}
try {
  if (command === 'controls') {
    console.log(JSON.stringify(await evaluate(`Array.from(document.querySelectorAll('button, textarea, input, [contenteditable="true"]')).map(el => ({tag:el.tagName,role:el.getAttribute('role'),label:el.getAttribute('aria-label'),title:el.getAttribute('title'),placeholder:el.getAttribute('placeholder'),testid:el.getAttribute('data-testid'),text:el.tagName==='BUTTON'?el.innerText.slice(0,80):undefined})).filter(x=>x.label||x.title||x.placeholder||x.testid||x.text)`), null, 2));
  } else if (command === 'eval') {
    console.log(JSON.stringify(await evaluate(args.join(' ')), null, 2));
  } else if (command === 'click') {
    const selector = args[0];
    console.log(await evaluate(`(() => {const el=document.querySelector(${JSON.stringify(selector)}); if(!el)return 'not found';el.click();return 'clicked';})()`));
  } else if (command === 'type') {
    await call('Input.insertText', { text: args.join(' ') }); console.log('typed');
  } else if (command === 'key') {
    const key = args[0];
    const code = key === 'Enter' ? 13 : key === 'Escape' ? 27 : undefined;
    await call('Input.dispatchKeyEvent', { type: 'keyDown', key, code: key, windowsVirtualKeyCode: code, nativeVirtualKeyCode: code });
    await call('Input.dispatchKeyEvent', { type: 'keyUp', key, code: key, windowsVirtualKeyCode: code, nativeVirtualKeyCode: code });
    console.log('key sent');
  } else if (command === 'screenshot') {
    const result = await call('Page.captureScreenshot', { format: 'png' });
    fs.writeFileSync(args[0] ?? '.local/desktop.png', Buffer.from(result.data, 'base64'));
    console.log('screenshot saved');
  } else if (command === 'close') {
    await call('Browser.close');
  } else {
    throw new Error('Commands: targets, controls, eval, click, type, key, screenshot, close');
  }
} finally {
  socket.close();
}
