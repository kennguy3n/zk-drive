//! Event types fanned into [`crate::engine::Engine::run`].

use std::path::PathBuf;

use zk_sync_api::Mutation;

/// One coalesced local-filesystem event after [`crate::watcher::Watcher`]
/// has dropped duplicates from rapid editor saves.
#[derive(Debug, Clone)]
pub enum LocalEvent {
    /// A file was created or modified.
    ///
    /// `size_bytes` and `content_hash` are sampled from the same
    /// open file descriptor inside [`crate::watcher::Watcher`] but
    /// are **not** guaranteed to reflect an atomically-consistent
    /// snapshot of the file: a concurrent writer can extend the
    /// file between `fstat` and the streaming hash read. Consumers
    /// that need a strictly-paired (size, hash) tuple must
    /// re-stat the file themselves under whatever locking they own;
    /// the engine treats `content_hash` as the source of truth for
    /// dedup (see `Engine::handle_local`) and is robust to
    /// transient size drift because `set_local_state` overwrites
    /// both fields atomically on the next event.
    Upsert {
        path: PathBuf,
        size_bytes: u64,
        content_hash: [u8; 32],
    },
    /// A file was removed.
    Delete { path: PathBuf },
    /// A file was renamed. NOTE: the bundled
    /// [`crate::watcher::Watcher`] does not currently emit this
    /// variant; on every platform we support, `notify` surfaces a
    /// rename as the source disappearing and the destination being
    /// created, and the watcher's flush logic drops the source
    /// (`File::open` fails because the source no longer exists) and
    /// emits an `Upsert` for the destination. The variant is kept
    /// because (a) future event sources -- e.g. an explicit "move"
    /// API exposed by the Tauri shell, or a platform watcher that
    /// supports notify's `Modify(Name(Both { from, to }))` payload
    /// -- need to push proper renames into the engine, and (b) the
    /// engine's catalogue-side handler is exercised by integration
    /// tests that synthesise the event directly.
    Rename { from: PathBuf, to: PathBuf },
}

/// One remote change feed mutation lifted into the engine.
///
/// The variants are subset & re-shaped slightly so the engine
/// doesn't have to switch on string-typed `kind` / `op` pairs
/// throughout its hot path. The original [`Mutation`] is preserved
/// inside [`RemoteEvent::Raw`] for any consumer that wants the
/// full record (e.g. a future audit overlay).
#[derive(Debug, Clone)]
pub enum RemoteEvent {
    FileCreated(Mutation),
    FileUpdated(Mutation),
    FileRenamed(Mutation),
    FileMoved(Mutation),
    FileDeleted(Mutation),
    FolderCreated(Mutation),
    /// `folder` + `update` -- e.g. a property change that isn't a
    /// rename / move. The engine currently ignores folder events but
    /// having the dedicated variant means a future folder-handling
    /// PR doesn't have to fish folder-updates back out of `Raw` and
    /// the variant survives the `unknown remote event` warn-log path.
    FolderUpdated(Mutation),
    FolderRenamed(Mutation),
    FolderMoved(Mutation),
    FolderDeleted(Mutation),
    PermissionChanged(Mutation),
    /// Forward-compat: a kind/op pair that this SDK build doesn't
    /// recognise. Logged and otherwise ignored so a server-side
    /// addition doesn't break older sync clients.
    Raw(Mutation),
}

impl RemoteEvent {
    pub fn from_mutation(m: Mutation) -> Self {
        use zk_sync_api::changefeed::*;
        match (m.kind.as_str(), m.op.as_str()) {
            (kind::FILE, op::CREATE) => RemoteEvent::FileCreated(m),
            (kind::FILE, op::UPDATE) => RemoteEvent::FileUpdated(m),
            (kind::FILE, op::RENAME) => RemoteEvent::FileRenamed(m),
            (kind::FILE, op::MOVE) => RemoteEvent::FileMoved(m),
            (kind::FILE, op::DELETE) => RemoteEvent::FileDeleted(m),
            (kind::FOLDER, op::CREATE) => RemoteEvent::FolderCreated(m),
            (kind::FOLDER, op::UPDATE) => RemoteEvent::FolderUpdated(m),
            (kind::FOLDER, op::RENAME) => RemoteEvent::FolderRenamed(m),
            (kind::FOLDER, op::MOVE) => RemoteEvent::FolderMoved(m),
            (kind::FOLDER, op::DELETE) => RemoteEvent::FolderDeleted(m),
            (kind::PERMISSION, _) => RemoteEvent::PermissionChanged(m),
            _ => RemoteEvent::Raw(m),
        }
    }

    pub fn mutation(&self) -> &Mutation {
        match self {
            RemoteEvent::FileCreated(m)
            | RemoteEvent::FileUpdated(m)
            | RemoteEvent::FileRenamed(m)
            | RemoteEvent::FileMoved(m)
            | RemoteEvent::FileDeleted(m)
            | RemoteEvent::FolderCreated(m)
            | RemoteEvent::FolderUpdated(m)
            | RemoteEvent::FolderRenamed(m)
            | RemoteEvent::FolderMoved(m)
            | RemoteEvent::FolderDeleted(m)
            | RemoteEvent::PermissionChanged(m)
            | RemoteEvent::Raw(m) => m,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::{TimeZone, Utc};
    use uuid::Uuid;

    fn mut_for(kind: &str, op: &str) -> Mutation {
        Mutation {
            sequence: 1,
            workspace_id: Uuid::nil(),
            actor_id: None,
            kind: kind.into(),
            op: op.into(),
            resource_id: Uuid::nil(),
            parent_id: None,
            name: String::new(),
            metadata: None,
            occurred_at: Utc.timestamp_opt(0, 0).unwrap(),
        }
    }

    #[test]
    fn from_mutation_maps_every_known_kind_op() {
        // Spot-check a few; the full coverage is exercised by the
        // Go-side exhaustiveness test on the changefeed side.
        assert!(matches!(
            RemoteEvent::from_mutation(mut_for("file", "create")),
            RemoteEvent::FileCreated(_)
        ));
        assert!(matches!(
            RemoteEvent::from_mutation(mut_for("folder", "delete")),
            RemoteEvent::FolderDeleted(_)
        ));
        assert!(matches!(
            RemoteEvent::from_mutation(mut_for("permission", "create")),
            RemoteEvent::PermissionChanged(_)
        ));
    }

    #[test]
    fn folder_update_lifts_to_folder_updated() {
        // Regression: previously `(folder, update)` fell through to
        // `Raw` because `FolderUpdated` didn't exist as a variant.
        let m = mut_for("folder", "update");
        assert!(matches!(
            RemoteEvent::from_mutation(m),
            RemoteEvent::FolderUpdated(_)
        ));
    }

    #[test]
    fn unknown_kind_falls_into_raw() {
        let m = mut_for("future_kind", "create");
        assert!(matches!(RemoteEvent::from_mutation(m), RemoteEvent::Raw(_)));
    }
}
