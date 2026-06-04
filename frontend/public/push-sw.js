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
  event.waitUntil(
    clients.matchAll({ type: "window", includeUncontrolled: true }).then((windowClients) => {
      // If a ZK Drive tab is currently visible AND focused, the user is
      // actively in the app and already received this notification over
      // the live WebSocket (the in-app toast). The server-side fan-out
      // is intentionally best-effort and replica-local ("favour delivery
      // over suppression"), so an online user — especially in a
      // multi-replica deployment where IsConnected only sees the local
      // hub — can still get a push. Showing a native OS notification on
      // top of the in-app one would be a visible duplicate, so suppress
      // it here where we can actually observe foreground state. When no
      // client is visible (tab backgrounded, minimised, or closed) we
      // show the notification as normal — the whole point of web push.
      const appInForeground = windowClients.some(
        (c) => c.visibilityState === "visible" && c.focused,
      );
      if (appInForeground) {
        return undefined;
      }
      return self.registration.showNotification(title, options);
    }),
  );
});

// isEditorPath reports whether a pathname renders the TipTap/Yjs document
// editor, where blindly navigating an open tab could discard unsaved
// edits. App.tsx mounts DocumentEditorPage on TWO routes, so both must be
// recognised: the canonical "/drive/document/:id" and the "/documents/:id/edit"
// alias used by the file-list Edit button. Missing either lets the click
// handler reuse (and navigate away from) an active editor tab.
function isEditorPath(pathname) {
  return (
    pathname.startsWith("/drive/document/") ||
    (pathname.startsWith("/documents/") && pathname.endsWith("/edit"))
  );
}

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
      const reusable = focusable.find((c) => !isEditorPath(pathnameOf(c.url)));
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
