import { useState } from "react";

import * as shell from "../api/shell";
import type { WorkspaceState } from "../types";

/**
 * Conflict-resolution dialog.
 *
 * The shell reports the *count* of conflicted files per workspace via
 * `Summary.conflict`; an individual-file resolution API
 * (`ResolveConflict`) is not yet exposed by the SDK `Command` surface
 * (see `src-tauri/src/commands.rs`). This dialog therefore offers the
 * last-writer-wins choices the engine supports and surfaces the
 * `Unsupported` error verbatim until the SDK grows the command — so
 * the UX contract is in place without stubbing SDK behaviour.
 */
export default function ConflictDialog({
  workspace,
  onClose,
  onResolved,
}: {
  workspace: WorkspaceState;
  onClose: () => void;
  onResolved: () => Promise<void> | void;
}) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function resolve(resolution: "local" | "remote") {
    setBusy(true);
    setError(null);
    try {
      // file_id is empty: this targets the workspace-wide policy until
      // per-file resolution lands in the SDK.
      await shell.resolveConflict(workspace.workspace_id, "", resolution);
      await onResolved();
      onClose();
    } catch (err) {
      setError(formatError(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>Resolve conflicts — {workspace.label}</h3>
        <p className="muted">
          {workspace.summary.conflict} file
          {workspace.summary.conflict === 1 ? "" : "s"} changed both locally and remotely. Choose
          which copy to keep.
        </p>

        <div className="modal-actions">
          <button disabled={busy} onClick={() => resolve("local")}>
            Keep local
          </button>
          <button disabled={busy} onClick={() => resolve("remote")}>
            Keep remote
          </button>
        </div>

        {error && <p className="error">{error}</p>}

        <button className="ghost close" onClick={onClose}>
          Close
        </button>
      </div>
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
