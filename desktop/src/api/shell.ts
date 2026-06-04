// Typed wrappers over the Tauri command surface (`src-tauri/src/commands.rs`)
// and the `"sync"` event channel (`src-tauri/src/events.rs`).
//
// Every function maps 1:1 to a `#[tauri::command]`; argument keys are
// camelCase because Tauri converts them to the Rust handlers' snake_case
// parameter names automatically.

import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";

import type { ShellEvent, TrayState, WorkspaceState } from "../types";

export type Provider = "google" | "microsoft";
export type FolderPolicy = "ignore" | "online" | "offline";

export function listWorkspaces(): Promise<WorkspaceState[]> {
  return invoke("list_workspaces");
}

export function getStatus(workspaceId: string): Promise<WorkspaceState> {
  return invoke("get_status", { workspaceId });
}

export function getTrayState(): Promise<TrayState> {
  return invoke("get_tray_state");
}

export function addWorkspace(label: string, root: string): Promise<WorkspaceState> {
  return invoke("add_workspace", { label, root });
}

export function removeWorkspace(workspaceId: string): Promise<void> {
  return invoke("remove_workspace", { workspaceId });
}

export function removeLocalCache(workspaceId: string): Promise<void> {
  return invoke("remove_local_cache", { workspaceId });
}

export function pauseSync(workspaceId: string): Promise<void> {
  return invoke("pause_sync", { workspaceId });
}

export function resumeSync(workspaceId: string): Promise<void> {
  return invoke("resume_sync", { workspaceId });
}

// ---- Auth ----------------------------------------------------------

export function authStatus(): Promise<boolean> {
  return invoke("auth_status");
}

export function beginLogin(provider: Provider): Promise<boolean> {
  return invoke("begin_login", { provider });
}

export function logout(): Promise<void> {
  return invoke("logout");
}

// ---- Selective sync / conflict (SDK-pending; see commands.rs) ------

export function setFolderPolicy(
  workspaceId: string,
  folderId: string,
  policy: FolderPolicy,
): Promise<void> {
  return invoke("set_folder_policy", { workspaceId, folderId, policy });
}

export function resolveConflict(
  workspaceId: string,
  fileId: string,
  resolution: "local" | "remote",
): Promise<void> {
  return invoke("resolve_conflict", { workspaceId, fileId, resolution });
}

// ---- Events --------------------------------------------------------

/** Subscribe to the shell event stream. Returns an unlisten fn. */
export function onSyncEvent(handler: (ev: ShellEvent) => void): Promise<UnlistenFn> {
  return listen<ShellEvent>("sync", (e) => handler(e.payload));
}
