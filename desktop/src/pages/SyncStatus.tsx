import { useMemo, useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";

import * as shell from "../api/shell";
import ConflictDialog from "../components/ConflictDialog";
import {
  healthLabel,
  pending,
  type WorkspaceState,
} from "../types";

interface Props {
  workspaces: WorkspaceState[];
  onChange: () => Promise<void> | void;
}

/**
 * Sync dashboard + workspace-binding page: lists every bound
 * workspace with its live health / progress, and a form to bind a new
 * local folder to a workspace.
 */
export default function SyncStatus({ workspaces, onChange }: Props) {
  const [label, setLabel] = useState("");
  const [root, setRoot] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [conflictFor, setConflictFor] = useState<WorkspaceState | null>(null);

  async function pickFolder() {
    const picked = await open({ directory: true, multiple: false, title: "Choose a folder to sync" });
    if (typeof picked === "string") {
      setRoot(picked);
      if (!label) setLabel(picked.split(/[\\/]/).filter(Boolean).pop() ?? "");
    }
  }

  async function bind(e: React.FormEvent) {
    e.preventDefault();
    if (!label.trim() || !root.trim()) return;
    setBusy(true);
    setError(null);
    try {
      await shell.addWorkspace(label.trim(), root.trim());
      setLabel("");
      setRoot("");
      await onChange();
    } catch (err) {
      setError(formatError(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="page">
      <form className="bind-form" onSubmit={bind}>
        <h2>Bind a folder</h2>
        <div className="row">
          <input
            placeholder="Workspace label"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
          />
          <input
            placeholder="Local folder path"
            value={root}
            onChange={(e) => setRoot(e.target.value)}
          />
          <button type="button" className="ghost" onClick={pickFolder}>
            Browse…
          </button>
          <button type="submit" disabled={busy || !label.trim() || !root.trim()}>
            {busy ? "Binding…" : "Add workspace"}
          </button>
        </div>
        {error && <p className="error">{error}</p>}
      </form>

      <h2>Workspaces</h2>
      {workspaces.length === 0 ? (
        <p className="muted">No workspaces yet. Bind a folder above to start syncing.</p>
      ) : (
        <ul className="ws-list">
          {workspaces.map((ws) => (
            <WorkspaceRow
              key={ws.workspace_id}
              ws={ws}
              onChange={onChange}
              onResolve={() => setConflictFor(ws)}
            />
          ))}
        </ul>
      )}

      {conflictFor && (
        <ConflictDialog
          workspace={conflictFor}
          onClose={() => setConflictFor(null)}
          onResolved={onChange}
        />
      )}
    </div>
  );
}

function WorkspaceRow({
  ws,
  onChange,
  onResolve,
}: {
  ws: WorkspaceState;
  onChange: () => Promise<void> | void;
  onResolve: () => void;
}) {
  const running = ws.health !== "stopped";
  const total = useMemo(() => Math.max(ws.summary.total_files, 1), [ws.summary.total_files]);
  const done = ws.summary.up_to_date;
  const pct = Math.round((done / total) * 100);

  async function toggle() {
    if (running) await shell.pauseSync(ws.workspace_id);
    else await shell.resumeSync(ws.workspace_id);
    await onChange();
  }

  async function remove() {
    await shell.removeWorkspace(ws.workspace_id);
    await onChange();
  }

  return (
    <li className="ws-row">
      <div className="ws-head">
        <span className={`dot status-${ws.health}`} aria-hidden />
        <div className="ws-meta">
          <strong>{ws.label}</strong>
          <span className="muted path">{ws.root}</span>
        </div>
        <span className={`status-tag status-${ws.health}`}>{healthLabel(ws.health)}</span>
      </div>

      <div className="progress" aria-label={`${pct}% synced`}>
        <div className="progress-bar" style={{ width: `${pct}%` }} />
      </div>

      <div className="ws-counts muted">
        {ws.summary.up_to_date} synced · {pending(ws.summary)} pending
        {ws.summary.conflict > 0 ? ` · ${ws.summary.conflict} conflicts` : ""}
      </div>

      {ws.last_error && <div className="error small">{ws.last_error}</div>}

      <div className="ws-actions">
        <button onClick={toggle}>{running ? "Pause" : "Resume"}</button>
        {ws.summary.conflict > 0 && (
          <button className="warn" onClick={onResolve}>
            Resolve conflicts
          </button>
        )}
        <button className="danger" onClick={remove}>
          Remove
        </button>
      </div>
    </li>
  );
}

function formatError(err: unknown): string {
  if (err && typeof err === "object" && "detail" in err) {
    const detail = (err as { detail: unknown }).detail;
    return typeof detail === "string" ? detail : JSON.stringify(detail);
  }
  return String(err);
}
