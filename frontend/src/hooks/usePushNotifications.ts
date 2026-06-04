import { useEffect } from "react";
import { getVapidPublicKey, registerPushSubscription } from "../api/client";

// urlBase64ToUint8Array converts the server's base64url-encoded VAPID
// public key into the Uint8Array applicationServerKey the browser
// PushManager expects.
function urlBase64ToUint8Array(base64String: string): Uint8Array<ArrayBuffer> {
  const padding = "=".repeat((4 - (base64String.length % 4)) % 4);
  const base64 = (base64String + padding).replace(/-/g, "+").replace(/_/g, "/");
  const raw = window.atob(base64);
  const buffer = new ArrayBuffer(raw.length);
  const output = new Uint8Array(buffer);
  for (let i = 0; i < raw.length; i++) {
    output[i] = raw.charCodeAt(i);
  }
  return output;
}

// applicationServerKeyMatches reports whether an existing browser
// PushSubscription was created with the same VAPID public key the
// server is currently advertising. A push subscription is permanently
// bound to the applicationServerKey it was created with: after the
// operator rotates VAPID keys, a reused old subscription causes the
// push service to reject deliveries (the server signs the request with
// the NEW private key, but the subscription expects the OLD public
// key), and those 403s are not the 410/404 the server auto-prunes on.
// Detecting the mismatch here lets us drop the stale subscription and
// re-subscribe against the current key.
function applicationServerKeyMatches(
  subscription: PushSubscription,
  key: Uint8Array<ArrayBuffer>,
): boolean {
  const existingKey = subscription.options.applicationServerKey;
  if (!existingKey) {
    return false;
  }
  const existing = new Uint8Array(existingKey as ArrayBuffer);
  if (existing.length !== key.length) {
    return false;
  }
  for (let i = 0; i < existing.length; i++) {
    if (existing[i] !== key[i]) {
      return false;
    }
  }
  return true;
}

function pushSupported(): boolean {
  return (
    typeof window !== "undefined" &&
    "serviceWorker" in navigator &&
    "PushManager" in window &&
    "Notification" in window
  );
}

// ensurePushSubscription subscribes through the active service worker
// registration and registers the resulting PushSubscription with the
// server. It is a no-op when the browser lacks push support, the user
// has denied permission, or the server has web push disabled (VAPID
// 501).
//
// promptIfDefault controls whether to call Notification.requestPermission
// when the user has not yet decided. The automatic on-login path passes
// false: prompting outside a user gesture is suppressed (Chrome's
// "quieter" UI) or blocked outright (Safari), which trains users to
// dismiss the prompt and tanks the grant rate. We instead surface an
// explicit "Enable notifications" control (enablePushNotifications),
// which passes true so the prompt fires inside a real click handler.
// When permission is already "granted" both paths (re)subscribe so a
// returning user's subscription is kept current (e.g. after VAPID key
// rotation) without any prompt.
// Resolves true only when a subscription was actually registered with the
// server; false for every no-op exit (unsupported, not granted, web push
// disabled). Callers use this to distinguish "permission granted AND
// subscribed" from "permission granted but registration failed" so the UI
// doesn't claim push is on when it isn't.
async function ensurePushSubscription(promptIfDefault: boolean): Promise<boolean> {
  if (!pushSupported()) {
    return false;
  }
  if (Notification.permission === "default") {
    if (!promptIfDefault) {
      // No decision yet and we're not allowed to prompt here (no user
      // gesture). Leave it to the explicit Enable-notifications control.
      return false;
    }
    const permission = await Notification.requestPermission();
    if (permission !== "granted") {
      return false;
    }
  }
  if (Notification.permission !== "granted") {
    return false;
  }

  let publicKey: string;
  try {
    publicKey = await getVapidPublicKey();
  } catch {
    // 501 (web push disabled) or network error — nothing to subscribe to.
    return false;
  }
  if (!publicKey) {
    return false;
  }

  const applicationServerKey = urlBase64ToUint8Array(publicKey);
  const registration = await navigator.serviceWorker.ready;

  // Reuse an existing subscription only when it was minted with the
  // current VAPID key. After a key rotation the old subscription is
  // unusable (deliveries 403), so drop it and create a fresh one.
  let subscription = await registration.pushManager.getSubscription();
  if (subscription && !applicationServerKeyMatches(subscription, applicationServerKey)) {
    await subscription.unsubscribe();
    subscription = null;
  }
  if (!subscription) {
    subscription = await registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey,
    });
  }

  await registerPushSubscription(subscription.toJSON() as PushSubscriptionJSON);
  return true;
}

// usePushNotifications wires browser Web Push for the logged-in user.
// Pass the current auth token; the subscription flow runs once whenever
// the user becomes authenticated. Failures are swallowed (logged) so a
// push-service hiccup never breaks rendering — in-app + WebSocket
// notifications remain the source of truth.
//
// On login we only (re)subscribe when permission is ALREADY granted; we
// never auto-prompt, because the request would not be tied to a user
// gesture. First-time opt-in goes through enablePushNotifications, wired
// to an explicit "Enable notifications" button.
export function usePushNotifications(token: string | null): void {
  useEffect(() => {
    if (!token) {
      return;
    }
    ensurePushSubscription(false).catch((err) => {
      // eslint-disable-next-line no-console
      console.warn("web push subscription failed", err);
    });
  }, [token]);
}

// EnablePushResult reports both the terminal permission and whether a
// subscription was actually registered. The two can diverge: the user may
// grant the prompt (permission "granted") but the follow-up VAPID fetch /
// registration can still fail (server 501, network error), leaving push
// not actually wired. The caller needs both to avoid showing push as "on"
// when only the permission — not the subscription — succeeded.
export interface EnablePushResult {
  permission: NotificationPermission;
  subscribed: boolean;
}

// enablePushNotifications is the gesture-triggered opt-in: call it from a
// click handler (the "Enable notifications" button) so the browser's
// permission prompt is allowed to appear. Resolves to the terminal
// permission plus whether a subscription was actually registered; resolves
// to denied/false when the browser lacks push support. Subscription
// failures after a granted prompt are swallowed (logged) like the on-login
// path, but surface as subscribed=false so the caller can keep the opt-in
// control visible for a retry instead of silently claiming success.
export async function enablePushNotifications(): Promise<EnablePushResult> {
  if (!pushSupported()) {
    return { permission: "denied", subscribed: false };
  }
  let subscribed = false;
  try {
    subscribed = await ensurePushSubscription(true);
  } catch (err) {
    // eslint-disable-next-line no-console
    console.warn("web push subscription failed", err);
  }
  return { permission: Notification.permission, subscribed };
}

// pushPermissionState reports the current opt-in state for UI gating
// without triggering any prompt. Returns "unsupported" when the browser
// can't do web push at all.
export function pushPermissionState(): NotificationPermission | "unsupported" {
  if (!pushSupported()) {
    return "unsupported";
  }
  return Notification.permission;
}
