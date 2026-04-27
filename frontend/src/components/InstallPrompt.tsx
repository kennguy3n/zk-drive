import { useEffect, useState } from "react";

// Subset of the BeforeInstallPromptEvent surface we use. The DOM lib does
// not declare it, so we keep just the bits we need.
interface BeforeInstallPromptEvent extends Event {
  readonly platforms: string[];
  prompt(): Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed"; platform: string }>;
}

const DISMISS_KEY = "zk-drive:install-prompt-dismissed";

// InstallPrompt listens for the browser's beforeinstallprompt event and
// renders a dismissible banner so users on supported browsers can install
// ZK Drive as a PWA. Once dismissed (or installed), the banner stays
// hidden for the rest of the session via localStorage.
export default function InstallPrompt() {
  const [deferred, setDeferred] = useState<BeforeInstallPromptEvent | null>(
    null,
  );
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    if (typeof window === "undefined") return;
    if (window.localStorage.getItem(DISMISS_KEY) === "1") return;

    const handler = (e: Event) => {
      e.preventDefault();
      setDeferred(e as BeforeInstallPromptEvent);
      setVisible(true);
    };
    const installed = () => {
      setVisible(false);
      setDeferred(null);
    };

    window.addEventListener("beforeinstallprompt", handler);
    window.addEventListener("appinstalled", installed);
    return () => {
      window.removeEventListener("beforeinstallprompt", handler);
      window.removeEventListener("appinstalled", installed);
    };
  }, []);

  const handleInstall = async () => {
    if (!deferred) return;
    try {
      await deferred.prompt();
      await deferred.userChoice;
    } finally {
      setDeferred(null);
      setVisible(false);
    }
  };

  const handleDismiss = () => {
    try {
      window.localStorage.setItem(DISMISS_KEY, "1");
    } catch {
      // ignore storage failures (private mode, etc.)
    }
    setVisible(false);
  };

  if (!visible || !deferred) return null;

  return (
    <div
      role="dialog"
      aria-label="Install ZK Drive"
      style={{
        position: "fixed",
        left: 16,
        right: 16,
        bottom: 16,
        zIndex: 1000,
        margin: "0 auto",
        maxWidth: 480,
        padding: "12px 16px",
        background: "#ffffff",
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        boxShadow: "0 6px 24px rgba(15, 23, 42, 0.12)",
        display: "flex",
        alignItems: "center",
        gap: 12,
        fontSize: 13,
        color: "#1f2937",
      }}
    >
      <span style={{ flex: 1 }}>Install ZK Drive for quick access</span>
      <button
        type="button"
        onClick={handleInstall}
        style={{
          padding: "6px 12px",
          background: "#2563eb",
          color: "white",
          border: "none",
          borderRadius: 4,
          fontSize: 13,
        }}
      >
        Install
      </button>
      <button
        type="button"
        onClick={handleDismiss}
        aria-label="Dismiss install prompt"
        style={{
          padding: "6px 10px",
          background: "transparent",
          color: "#6b7280",
          border: "1px solid #e5e7eb",
          borderRadius: 4,
          fontSize: 13,
        }}
      >
        Not now
      </button>
    </div>
  );
}
