import { useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";

import * as shell from "../api/shell";
import type { WorkspaceState } from "../types";

interface Props {
  workspaces: WorkspaceState[];
  onChange: () => Promise<void> | void;
}

const POLICIES: shell.FolderPolicy[] = ["offline", "online", "ignore"];

const POLICY_HELP: Record<shell.FolderPolicy, string> = {
  offline: "Keep a full local copy (download everything).",
  online: "Keep metadata only; fetch file contents on demand.",
  ignore: "Don't sync this folder at all.",
};

/**
 * Settings / selective-sync page. Lets the user set a per-folder
 * [`SyncPolicy`] (offline / online / ignore) for any sub-folder of a
 * workspace, and manage the local cache.
 */
export default function Settings({ workspaces, onChange }: Props) {
  return (
    <div className="page">
      <h2>Selective sync</h2>
      {workspaces.length === 0 ? (
        <p className="muted">Bind a workspace first to configure selective sync.</p>
      ) : (
        workspaces.map((ws) => <SelectiveSync key={ws.workspace_id} ws={ws} onChange={onChange} />)
      )}

      <h2>Updates</h2>
      <p className="muted">
        ZK Drive checks for updates automatically via the Tauri updater (GitHub Releases). New
        versions install on the next launch.
      </p>
    </div>
  );
}

function SelectiveSync({
  ws,
  onChange,
}: {
  ws: WorkspaceState;
  onChange: () => Promise<void> | void;
}) {
  const [folder, setFolder] = useState("");
  const [policy, setPolicy] = useState<shell.FolderPolicy>("offline");
  const [note, setNote] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function pickSub() {
    const picked = await open({
      directory: true,
      multiple: false,
      defaultPath: ws.root,
      title: `Choose a folder inside ${ws.label}`,
    });
    if (typeof picked === "string") setFolder(picked);
  }

  async function apply() {
    if (!folder) return;
    setBusy(true);
    setNote(null);
    try {
      await shell.setFolderPolicy(ws.workspace_id, folder, policy);
      setNote("Policy applied.");
      await onChange();
    } catch (err) {
      // The SDK does not expose SetFolderPolicy yet; show the
      // structured reason rather than failing silently.
      setNote(formatError(err));
    } finally {
      setBusy(false);
    }
  }

  async function clearCache() {
    setBusy(true);
    setNote(null);
    try {
      await shell.removeLocalCache(ws.workspace_id);
      setNote("Local cache cleared.");
      await onChange();
    } catch (err) {
      setNote(formatError(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <div className="ws-head">
        <strong>{ws.label}</strong>
        <span className="muted path">{ws.root}</span>
      </div>

      <div className="row">
        <input
          placeholder="Folder path"
          value={folder}
          onChange={(e) => setFolder(e.target.value)}
        />
        <button type="button" className="ghost" onClick={pickSub}>
          Browse…
        </button>
        <select value={policy} onChange={(e) => setPolicy(e.target.value as shell.FolderPolicy)}>
          {POLICIES.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
        <button disabled={busy || !folder} onClick={apply}>
          Apply
        </button>
      </div>
      <p className="muted small">{POLICY_HELP[policy]}</p>

      <div className="ws-actions">
        <button className="danger" disabled={busy} onClick={clearCache}>
          Clear local cache
        </button>
      </div>

      {note && <p className="note small">{note}</p>}
    </div>
  );
}

function formatError(err: unknown): string {
  if (err && typeof err === "object" && "detail" in err) {
    const detail = (err as { detail: unknown }).detail;
    return typeof detail === "string" ? detail : JSON.stringify(detail);
  }
  return String(err);
}
