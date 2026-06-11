import { useEffect, useState } from "react";
import { WifiOff } from "lucide-react";

// useOnlineStatus tracks the browser's connectivity. Separated from the
// indicator so other components (e.g. upload retry logic) can reuse it.
export function useOnlineStatus(): boolean {
  const [online, setOnline] = useState<boolean>(
    typeof navigator === "undefined" ? true : navigator.onLine,
  );
  useEffect(() => {
    const up = () => setOnline(true);
    const down = () => setOnline(false);
    window.addEventListener("online", up);
    window.addEventListener("offline", down);
    return () => {
      window.removeEventListener("online", up);
      window.removeEventListener("offline", down);
    };
  }, []);
  return online;
}

// OfflineIndicator renders a fixed banner at the top of the viewport when
// the browser loses connectivity, so the user understands why actions are
// failing instead of seeing silent errors. role="status" announces the
// state change to screen readers.
export function OfflineIndicator() {
  const online = useOnlineStatus();
  if (online) return null;
  return (
    <div
      role="status"
      aria-live="assertive"
      className="fixed inset-x-0 top-0 z-[200] flex items-center justify-center gap-2 bg-warning px-4 py-1.5 text-sm font-medium text-black shadow"
    >
      <WifiOff className="h-4 w-4" aria-hidden="true" />
      You&rsquo;re offline — changes will sync when the connection returns.
    </div>
  );
}
