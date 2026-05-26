// Self-unregistering service worker.
// Clears all cached assets and unregisters so the app uses the network directly.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', async () => {
  const keys = await caches.keys();
  await Promise.all(keys.map((k) => caches.delete(k)));
  await self.clients.claim();
  await self.registration.unregister();
});
