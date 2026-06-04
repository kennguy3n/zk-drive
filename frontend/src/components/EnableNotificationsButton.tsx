import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  enablePushNotifications,
  pushPermissionState,
} from "../hooks/usePushNotifications";

// EnableNotificationsButton renders an explicit opt-in control for Web
// Push. It only appears while the user has not yet decided
// (Notification.permission === "default") AND the browser supports push,
// because that is the only state where a click can meaningfully open the
// permission prompt. Gating the prompt behind this real user gesture
// (rather than auto-prompting on login) avoids Chrome's "quieter" UI and
// Safari's outright block, and keeps the grant rate high.
//
// Once the user grants or denies, the component reflects the terminal
// state briefly (granted) or hides itself (the browser won't re-prompt
// after a denial anyway), so it never nags.
export default function EnableNotificationsButton({
  style,
}: {
  style?: React.CSSProperties;
}) {
  const { t } = useTranslation();
  const [state, setState] = useState<
    NotificationPermission | "unsupported"
  >(() => pushPermissionState());
  const [busy, setBusy] = useState(false);

  // Permission can change in another tab or via browser settings; resync
  // on focus so the control doesn't show a stale state.
  useEffect(() => {
    const resync = () => setState(pushPermissionState());
    window.addEventListener("focus", resync);
    return () => window.removeEventListener("focus", resync);
  }, []);

  const onClick = useCallback(async () => {
    setBusy(true);
    try {
      const { permission, subscribed } = await enablePushNotifications();
      // If the prompt was granted but registration didn't actually
      // complete (VAPID fetch / network error), keep the button visible
      // (treat as undecided) so the user can retry rather than believing
      // push is on. usePushNotifications also retries on the next login.
      setState(permission === "granted" && !subscribed ? "default" : permission);
    } finally {
      setBusy(false);
    }
  }, []);

  // Unsupported browsers and the already-granted state need no button:
  // granted users are (re)subscribed automatically on login. A prior
  // denial can't be re-prompted from JS, so we hide rather than nag.
  if (state === "unsupported" || state === "granted" || state === "denied") {
    return null;
  }

  return (
    <button
      type="button"
      onClick={onClick}
      disabled={busy}
      style={style}
      title={t("notifications.enableTooltip")}
    >
      {t("notifications.enable")}
    </button>
  );
}
