import { useCallback, useEffect, useState } from "react";

import Login from "./pages/Login";
import SyncStatus from "./pages/SyncStatus";
import Settings from "./pages/Settings";
import * as shell from "./api/shell";
import { healthLabel, type TrayState, type WorkspaceState } from "./types";

type Tab = "status" | "settings";

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [tab, setTab] = useState<Tab>("status");
  const [workspaces, setWorkspaces] = useState<WorkspaceState[]>([]);
  const [tray, setTray] = useState<TrayState | null>(null);

  // Pull the full state from the shell. Cheap (in-memory on the Rust
  // side) so we call it on mount and on every shell event rather than
  // maintaining a parallel reducer in JS.
  const refresh = useCallback(async () => {
    try {
      const [ws, t] = await Promise.all([shell.listWorkspaces(), shell.getTrayState()]);
      setWorkspaces(ws);
      setTray(t);
    } catch (err) {
      console.error("refresh failed", err);
    }
  }, []);

  // Resolve auth state on launch.
  useEffect(() => {
    shell
      .authStatus()
      .then(setAuthed)
      .catch(() => setAuthed(false));
  }, []);

  // Once authed, load state and subscribe to the live event stream.
  useEffect(() => {
    if (!authed) return;
    void refresh();
    const unlisten = shell.onSyncEvent(() => {
      void refresh();
    });
    return () => {
      void unlisten.then((fn) => fn());
    };
  }, [authed, refresh]);

  if (authed === null) {
    return <div className="splash">Loading…</div>;
  }

  if (!authed) {
    return <Login onAuthenticated={() => setAuthed(true)} />;
  }

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="logo" aria-hidden>
            ◆
          </span>
          ZK Drive
        </div>
        {tray && (
          <span className={`status-pill status-${tray.health}`}>
            {healthLabel(tray.health)}
            {tray.total_conflicts > 0 ? ` · ${tray.total_conflicts} conflicts` : ""}
            {tray.total_pending > 0 ? ` · ${tray.total_pending} pending` : ""}
          </span>
        )}
        <nav className="tabs">
          <button className={tab === "status" ? "active" : ""} onClick={() => setTab("status")}>
            Sync
          </button>
          <button
            className={tab === "settings" ? "active" : ""}
            onClick={() => setTab("settings")}
          >
            Settings
          </button>
        </nav>
        <button
          className="ghost"
          onClick={async () => {
            await shell.logout();
            setAuthed(false);
          }}
        >
          Sign out
        </button>
      </header>

      <main className="content">
        {tab === "status" ? (
          <SyncStatus workspaces={workspaces} onChange={refresh} />
        ) : (
          <Settings workspaces={workspaces} onChange={refresh} />
        )}
      </main>
    </div>
  );
}
