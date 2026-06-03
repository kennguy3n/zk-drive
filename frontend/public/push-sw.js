/* global self, clients */
// Web Push handlers for the ZK Drive PWA service worker.
//
// This script is imported into the Workbox-generated service worker via
// `workbox.importScripts` (see frontend/vite.config.ts). It runs in the
// service worker scope, so it has access to `self.registration` even when
// no tab is open — that's the whole point of web push.
//
// The server (internal/notification/webpush.go) sends a JSON payload of
// the shape { title, body, type?, url? }. We surface it as a native
// notification and, on click, focus an existing client or open the app.

self.addEventListener("push", (event) => {
  let payload = {};
  if (event.data) {
    try {
      payload = event.data.json();
    } catch (e) {
      payload = { title: "ZK Drive", body: event.data.text() };
    }
  }
  const title = payload.title || "ZK Drive";
  const options = {
    body: payload.body || "",
    icon: "/pwa-192x192.png",
    badge: "/pwa-192x192.png",
    // Collapse repeat notifications of the same type so a burst doesn't
    // stack; data carries the deep link for the click handler.
    tag: payload.type || "zk-drive-notification",
    data: { url: payload.url || "/drive" },
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const targetUrl = (event.notification.data && event.notification.data.url) || "/drive";
  event.waitUntil(
    clients.matchAll({ type: "window", includeUncontrolled: true }).then((windowClients) => {
      for (const client of windowClients) {
        if ("focus" in client) {
          client.navigate(targetUrl);
          return client.focus();
        }
      }
      if (clients.openWindow) {
        return clients.openWindow(targetUrl);
      }
      return undefined;
    }),
  );
});
