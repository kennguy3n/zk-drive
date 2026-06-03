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

// EDITOR_PATH_PREFIX marks routes (the TipTap/Yjs document editor) where
// blindly navigating an open tab could discard unsaved edits. The click
// handler avoids redirecting such tabs.
const EDITOR_PATH_PREFIX = "/drive/document/";

function pathnameOf(url) {
  try {
    return new URL(url).pathname;
  } catch (e) {
    return "";
  }
}

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const targetUrl = (event.notification.data && event.notification.data.url) || "/drive";
  event.waitUntil(
    clients.matchAll({ type: "window", includeUncontrolled: true }).then((windowClients) => {
      const focusable = windowClients.filter((c) => "focus" in c);

      // 1) A tab already on the target page: just focus it, no navigation.
      const alreadyThere = focusable.find((c) => pathnameOf(c.url) === targetUrl);
      if (alreadyThere) {
        return alreadyThere.focus();
      }

      // 2) Reuse a tab that ISN'T mid-edit so we don't blow away unsaved
      // document changes by navigating away from the editor.
      const reusable = focusable.find((c) => !pathnameOf(c.url).startsWith(EDITOR_PATH_PREFIX));
      if (reusable) {
        return Promise.resolve(reusable.navigate(targetUrl)).then(() => reusable.focus());
      }

      // 3) Every open tab is an editor (or none are open): open a new
      // window rather than disrupting in-progress editing.
      if (clients.openWindow) {
        return clients.openWindow(targetUrl);
      }
      return undefined;
    }),
  );
});
