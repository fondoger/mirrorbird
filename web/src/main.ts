import './style.css';
import { CompanionStore, type EventStream } from './store.ts';
import { HttpApi } from './transport.ts';
import { CompanionView } from './view.ts';
import { OfflineCache } from './cache.ts';

const root = document.getElementById('app');
if (!root) throw new Error('Missing application root');
let cache: OfflineCache | undefined;
try { cache = new OfflineCache(localStorage); }
catch { /* Storage availability is reported in the connection details below. */ }
const store = new CompanionStore(new HttpApi(), {
  events: (): EventStream => {
    const source = new EventSource('/api/events', { withCredentials: true });
    return {
      addEventListener: (type, callback) => source.addEventListener(type, (event) => callback({ data: event instanceof MessageEvent && typeof event.data === 'string' ? event.data : undefined })),
      close: () => source.close(),
    };
  },
  uuid: () => crypto.randomUUID(),
  cache,
});
if (!cache) store.state.cacheError = '浏览器存储不可用，此设备无法保存离线内容。';
new CompanionView(root, store);
void store.start();
if (!navigator.onLine) store.offline();

if ('serviceWorker' in navigator && import.meta.env.PROD) {
  void navigator.serviceWorker.register('/sw.js').catch(() => {
    store.notify('离线应用安装失败；联网功能仍可使用，请检查浏览器站点权限。', 'error');
  });
}

window.addEventListener('online', () => void store.recover());
window.addEventListener('offline', () => store.offline());
window.addEventListener('focus', () => void store.foreground());
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') void store.foreground();
});
window.addEventListener('pagehide', () => store.saveCache());
window.addEventListener('pageshow', (event) => { if (event.persisted) void store.recover(); });
