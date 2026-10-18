import './style.css';
import { CompanionStore, type EventStream } from './store.ts';
import { HttpApi } from './transport.ts';
import { CompanionView } from './view.ts';

const root = document.getElementById('app');
if (!root) throw new Error('Missing application root');
const store = new CompanionStore(new HttpApi(), {
  events: () => new EventSource('/api/events', { withCredentials: true }) as unknown as EventStream,
  uuid: () => crypto.randomUUID(),
});
new CompanionView(root, store);
void store.start();

window.addEventListener('online', () => void store.recover());
window.addEventListener('offline', () => store.offline());
window.addEventListener('focus', () => void store.recover());
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') void store.recover();
});
// A cached back/forward page must re-check the service lifetime rather than
// exposing a prior transcript. No service worker or browser storage is involved.
window.addEventListener('pagehide', () => { store.dispose(); root.replaceChildren(); });
window.addEventListener('pageshow', (event) => { if (event.persisted) window.location.reload(); });
