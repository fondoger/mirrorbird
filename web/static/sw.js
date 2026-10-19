/* Only the application shell is cached. Authentication, APIs and POSTs always use the network. */
const CACHE = 'mirrorbird-shell-73bc3d28659634d4';
self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const response = await fetch('/', { cache: 'reload', redirect: 'error' });
    if (!response.ok || !response.headers.get('content-type')?.includes('text/html')) throw new Error('Shell unavailable');
    const html = await response.clone().text();
    const assets = [...html.matchAll(/(?:src|href)="(\/(?:assets\/[^"]+|favicon\.svg|manifest\.webmanifest|icon-\d+\.png))"/g)].map((match) => match[1]);
    if (!assets.some((path) => path.startsWith('/assets/') && path.endsWith('.js'))) throw new Error('Not an application shell');
    const cache = await caches.open(CACHE);
    await cache.addAll([...new Set([...assets, '/icon-512.png'])]);
    await cache.put('/', response);
  })());
});
self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    for (const key of await caches.keys()) if (key.startsWith('mirrorbird-shell-') && key !== CACHE) await caches.delete(key);
    await self.clients.claim();
  })());
});
self.addEventListener('fetch', (event) => {
  const request = event.request, url = new URL(request.url);
  if (request.method !== 'GET' || url.origin !== self.location.origin || url.pathname.startsWith('/api/') || url.pathname.startsWith('/__preview/')) return;
  if (request.mode === 'navigate' && url.pathname === '/') {
    event.respondWith(fetch(request).catch(async () => {
      const shell = await caches.match('/');
      if (!shell) throw new Error('No offline shell');
      return shell;
    }));
  } else if (/^\/(?:assets\/|favicon\.svg$|manifest\.webmanifest$|icon-\d+\.png$)/.test(url.pathname)) {
    event.respondWith(caches.match(request).then((cached) => cached || fetch(request)));
  }
});
