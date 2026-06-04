// TypeScript mirrors of the serde-shaped types the desktop-shell
// crate emits. These intentionally match the `#[serde(...)]` wire
// formats in `sdk/crates/desktop-shell` so events / command replies
// deserialize without a translation layer:
//   * SyncHealth / TaskKind: `rename_all = "snake_case"`
//   * ShellEvent / Command(Result): `tag = "type", content = "data"`

export type SyncHealth =
  | "stopped"
  | "starting"
  | "idle"
  | "syncing"
  | "conflict"
  | "error";

/** Count-by-status snapshot over a workspace's catalogue. */
export interface Summary {
  total_files: number;
  total_bytes: number;
  up_to_date: number;
  local_dirty: number;
  local_deleted: number;
  remote_dirty: number;
  remote_deleted: number;
  conflict: number;
  in_flight: number;
  evicted: number;
  cursor: number;
}

export interface WorkspaceState {
  workspace_id: string;
  label: string;
  root: string;
  health: SyncHealth;
  summary: Summary;
  last_error: string | null;
  last_updated: string;
}

export interface TrayState {
  health: SyncHealth;
  total_pending: number;
  total_conflicts: number;
  workspaces: number;
  workspaces_running: number;
  first_error: string | null;
}

export type TaskKind = "engine" | "poller" | "watcher" | "health_loop";

/** Mirror of `zk_sync_shell::ShellEvent`. */
export type ShellEvent =
  | { type: "workspace_added"; data: { workspace_id: string; label: string } }
  | { type: "workspace_removed"; data: { workspace_id: string } }
  | {
      type: "health_changed";
      data: { workspace_id: string; health: SyncHealth; reason: string | null };
    }
  | { type: "summary_changed"; data: { workspace_id: string; summary: Summary } }
  | { type: "tray_changed"; data: { tray: TrayState } }
  | {
      type: "task_failed";
      data: { workspace_id: string; task: TaskKind; message: string };
    };

/** Mirror of `crate::error::DesktopError` (the command error shape). */
export interface DesktopError {
  kind: "command" | "auth" | "unsupported" | "io" | "api";
  detail: unknown;
}

/** Number of files still needing work (matches `Summary::pending`). */
export function pending(s: Summary): number {
  return (
    s.local_dirty +
    s.local_deleted +
    s.remote_dirty +
    s.remote_deleted +
    s.conflict +
    s.in_flight
  );
}

/**
 * Mirror of the SDK's `SyncHealth::is_running()`
 * (sdk/crates/desktop-shell/src/state.rs): the workspace's background
 * task is supposed to be live. Deliberately excludes both `stopped`
 * and `error` so the UI's action mapping matches the engine's own
 * notion of "running" rather than re-deriving its own.
 */
export function isRunning(h: SyncHealth): boolean {
  return h === "starting" || h === "idle" || h === "syncing" || h === "conflict";
}

/** Human label + accent color for a health value. */
export function healthLabel(h: SyncHealth): string {
  switch (h) {
    case "idle":
      return "Up to date";
    case "syncing":
      return "Syncing";
    case "conflict":
      return "Conflicts";
    case "error":
      return "Error";
    case "starting":
      return "Starting";
    case "stopped":
      return "Paused";
  }
}
