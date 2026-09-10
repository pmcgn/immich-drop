// Minimal service worker for the admin PWA (login/menu). Exists only to
// satisfy browser installability checks — it deliberately does not cache
// anything, so the admin UI and its data always come straight from the
// network and can never go stale.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));
self.addEventListener('fetch', (event) => {
  event.respondWith(fetch(event.request));
});
