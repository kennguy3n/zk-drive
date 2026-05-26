//! Persistent app config — a JSON sidecar listing every workspace
//! the user has added to the shell.
//!
//! The shell reads this file on startup so workspaces survive a
//! desktop-app relaunch. The file is rewritten atomically (via the
//! standard write-tmp-then-rename pattern) on every mutation so a
//! crash mid-write never corrupts the registry.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// One row in the persistent registry. Mirrors the in-memory
/// [`crate::WorkspaceState`] minus the runtime-only fields
/// (`health`, `summary`, `last_error`, `last_updated`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct WorkspaceEntry {
    pub workspace_id: Uuid,
    pub label: String,
    pub root: PathBuf,
    /// Filesystem path the catalogue SQLite database lives at.
    /// Stored per-workspace so a future "import existing catalogue"
    /// flow can point at a sibling path instead of forcing the
    /// shell's default layout.
    pub catalogue_path: PathBuf,
    /// Set to `true` to start the workspace on shell launch. The
    /// frontend's per-workspace "Start at login" toggle writes
    /// this; the shell honours it via [`crate::App::resume_persisted`].
    #[serde(default)]
    pub autostart: bool,
}

/// Top-level config file.
///
/// `version` is currently `1`; future migrations bump it and we add
/// a `match` arm in [`AppConfig::load`]. Past Devin-style
/// `Workstream` reviews have flagged "schema versions never read"
/// as a smell, so we anchor the read path explicitly: any file we
/// can't recognise is rejected loudly rather than silently being
/// treated as the latest format.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AppConfig {
    pub version: u32,
    pub workspaces: Vec<WorkspaceEntry>,
}

impl Default for AppConfig {
    fn default() -> Self {
        Self {
            version: 1,
            workspaces: Vec::new(),
        }
    }
}

impl AppConfig {
    /// Read the config from `path`. Returns
    /// [`AppConfig::default`] (a fresh, empty registry) if the file
    /// doesn't exist, so a first-launch GUI doesn't have to special-
    /// case the missing-file path.
    ///
    /// Returns [`crate::ShellError`] for malformed JSON or an
    /// unrecognised `version` — we never silently coerce a future
    /// schema into the current one, because dropping unknown fields
    /// would lose the user's settings on a downgrade.
    pub fn load(path: impl AsRef<Path>) -> crate::Result<Self> {
        let path = path.as_ref();
        match std::fs::read_to_string(path) {
            Ok(s) => {
                let parsed: AppConfig = serde_json::from_str(&s)?;
                if parsed.version != 1 {
                    return Err(crate::ShellError::Other(format!(
                        "config at {} has unrecognised version {} (this build understands version 1)",
                        path.display(),
                        parsed.version
                    )));
                }
                Ok(parsed)
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Self::default()),
            Err(e) => Err(e.into()),
        }
    }

    /// Atomically write the config to `path`.
    ///
    /// Strategy:
    ///
    /// 1. Serialise to JSON with `serde_json::to_vec_pretty` —
    ///    pretty so an operator inspecting `~/.config/zk-sync/app.json`
    ///    gets readable output, and a deliberate format mismatch
    ///    surfaces in `git diff` if the file is checked in for
    ///    debugging.
    /// 2. Write to `<path>.tmp` and `rename` over `path`. POSIX
    ///    guarantees the rename is atomic on the same filesystem,
    ///    so a crash between the write and the rename leaves the
    ///    old file intact instead of a half-truncated config.
    pub fn save(&self, path: impl AsRef<Path>) -> crate::Result<()> {
        let path = path.as_ref();
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let tmp = path.with_extension("json.tmp");
        let bytes = serde_json::to_vec_pretty(self)?;
        std::fs::write(&tmp, &bytes)?;
        std::fs::rename(&tmp, path)?;
        Ok(())
    }

    /// Find a workspace by id. Returns `None` if not present.
    pub fn find(&self, workspace_id: Uuid) -> Option<&WorkspaceEntry> {
        self.workspaces
            .iter()
            .find(|w| w.workspace_id == workspace_id)
    }

    /// Insert a new workspace. Returns `Err` if a workspace with
    /// the same id is already present — the [`crate::App`] surface
    /// catches the duplicate first, so by the time we reach this
    /// path it's a programming bug; we still defensively guard the
    /// registry's invariant.
    pub fn insert(&mut self, entry: WorkspaceEntry) -> crate::Result<()> {
        if self.find(entry.workspace_id).is_some() {
            return Err(crate::ShellError::AlreadyRegistered(entry.workspace_id));
        }
        self.workspaces.push(entry);
        Ok(())
    }

    /// Remove a workspace by id. Returns the dropped entry so the
    /// caller can decide whether to also delete the catalogue file.
    pub fn remove(&mut self, workspace_id: Uuid) -> Option<WorkspaceEntry> {
        let idx = self
            .workspaces
            .iter()
            .position(|w| w.workspace_id == workspace_id)?;
        Some(self.workspaces.remove(idx))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    fn entry(id: Uuid) -> WorkspaceEntry {
        WorkspaceEntry {
            workspace_id: id,
            label: "Acme".into(),
            root: PathBuf::from("/tmp/acme"),
            catalogue_path: PathBuf::from("/tmp/acme/cat.db"),
            autostart: false,
        }
    }

    #[test]
    fn save_then_load_round_trips() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("app.json");
        let mut cfg = AppConfig::default();
        cfg.insert(entry(Uuid::nil())).unwrap();
        cfg.save(&path).unwrap();
        let loaded = AppConfig::load(&path).unwrap();
        assert_eq!(loaded, cfg);
    }

    #[test]
    fn load_missing_file_returns_default() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("does-not-exist.json");
        let cfg = AppConfig::load(&path).unwrap();
        assert_eq!(cfg, AppConfig::default());
    }

    #[test]
    fn load_rejects_unknown_version() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("app.json");
        std::fs::write(&path, r#"{"version":99,"workspaces":[]}"#).unwrap();
        let err = AppConfig::load(&path).unwrap_err();
        let msg = format!("{err}");
        assert!(
            msg.contains("unrecognised version 99"),
            "expected version-rejection message, got: {msg}"
        );
    }

    #[test]
    fn save_writes_via_tmp_then_rename() {
        // After a successful save() there must be no .tmp file
        // lying around -- the rename is supposed to consume it.
        let dir = tempdir().unwrap();
        let path = dir.path().join("app.json");
        let cfg = AppConfig::default();
        cfg.save(&path).unwrap();
        let tmp = path.with_extension("json.tmp");
        assert!(path.exists(), "primary file must exist after save");
        assert!(!tmp.exists(), "tmp file must be renamed away after save");
    }

    #[test]
    fn insert_rejects_duplicate_workspace_id() {
        let mut cfg = AppConfig::default();
        let id = Uuid::nil();
        cfg.insert(entry(id)).unwrap();
        let err = cfg.insert(entry(id)).unwrap_err();
        assert!(matches!(err, crate::ShellError::AlreadyRegistered(_)));
    }
}
