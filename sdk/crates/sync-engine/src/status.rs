//! Status snapshot surfaced by the CLI and Tauri tray.
//!
//! The snapshot is a point-in-time aggregate of the catalogue and
//! the connectivity state. It is generated on demand (not pushed)
//! because the CLI's `zk-sync status` is a one-shot command and the
//! tray's polling cadence is human-scale (seconds, not
//! milliseconds), so the cost of a fresh aggregate query per refresh
//! is negligible compared to keeping a denormalised counter cache.
//!
//! Counts are scoped to the catalogue's bound `workspace_id` (the
//! catalogue rejects opens for any other workspace, see
//! [`Catalogue::open`]). A single catalogue contains exactly one
//! workspace's rows, so there is no per-workspace filter to apply
//! here.

use serde::{Deserialize, Serialize};

use crate::catalogue::Catalogue;
use crate::connectivity::ConnectivityState;
use crate::Result;

/// Per-status row counts. Each field is a count of catalogue rows
/// in that [`SyncStatus`]. Surfaced as part of [`Snapshot`] so
/// operators can read "X pending uploads, Y pending downloads,
/// Z conflicts" at a glance.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct SyncStatusCounts {
    pub up_to_date: u64,
    pub local_dirty: u64,
    pub local_deleted: u64,
    pub remote_dirty: u64,
    pub remote_deleted: u64,
    pub conflict: u64,
    pub in_flight: u64,
    pub evicted: u64,
}

impl SyncStatusCounts {
    /// Sum across every variant. Equal to the catalogue's total
    /// row count; useful as a sanity check.
    pub fn total(&self) -> u64 {
        self.up_to_date
            .saturating_add(self.local_dirty)
            .saturating_add(self.local_deleted)
            .saturating_add(self.remote_dirty)
            .saturating_add(self.remote_deleted)
            .saturating_add(self.conflict)
            .saturating_add(self.in_flight)
            .saturating_add(self.evicted)
    }

    /// Rows that represent unfinished work (anything other than
    /// `UpToDate` or `Evicted`). The CLI uses this to summarise
    /// "5 changes pending sync" without enumerating each status.
    pub fn pending(&self) -> u64 {
        self.local_dirty
            .saturating_add(self.local_deleted)
            .saturating_add(self.remote_dirty)
            .saturating_add(self.remote_deleted)
            .saturating_add(self.conflict)
            .saturating_add(self.in_flight)
    }
}

/// One-shot status aggregate. Suitable for serialising to the CLI's
/// `--json` output, the Tauri shell's status panel, or a future
/// `/api/v1/agent/status` introspection endpoint.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Snapshot {
    /// Workspace this snapshot is for. Echoed back so the operator
    /// can confirm the CLI is talking to the right catalogue when
    /// multiple sync agents are configured.
    pub workspace_id: uuid::Uuid,
    /// Total on-disk bytes occupied by the local cache (the sum
    /// from [`Catalogue::total_cached_bytes`]).
    pub cached_bytes: u64,
    /// Configured disk quota in bytes, if any. `None` means the
    /// engine is configured with an unbounded local cache.
    pub disk_quota_bytes: Option<u64>,
    /// Number of pinned rows (always kept locally, never evicted).
    pub pinned_count: u64,
    /// Number of rows by [`SyncStatus`].
    pub status_counts: SyncStatusCounts,
    /// Network connectivity as seen by the engine. `Unknown` at
    /// startup before the first request lands.
    pub connectivity: ConnectivityStateOwned,
    /// True if `cached_bytes > disk_quota_bytes`. The tray UI
    /// shows a yellow indicator in this state.
    pub over_quota: bool,
}

/// `ConnectivityState` is `Copy`, but we need an owned form for
/// serde. This is a thin renaming so the wire schema stays
/// readable and we don't leak the implementation detail that the
/// underlying flag is an atomic.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConnectivityStateOwned {
    Unknown,
    Online,
    Offline,
}

impl From<ConnectivityState> for ConnectivityStateOwned {
    fn from(s: ConnectivityState) -> Self {
        match s {
            ConnectivityState::Unknown => ConnectivityStateOwned::Unknown,
            ConnectivityState::Online => ConnectivityStateOwned::Online,
            ConnectivityState::Offline => ConnectivityStateOwned::Offline,
        }
    }
}

impl Snapshot {
    /// Build a fresh snapshot from a catalogue handle.
    ///
    /// `disk_quota_bytes` is passed in rather than read from the
    /// catalogue because the quota is engine config, not persisted
    /// state -- the catalogue file is the same regardless of which
    /// quota policy the operator runs the agent under.
    ///
    /// `connectivity` is the engine's current connectivity flag
    /// (shared atomic). The snapshot captures its value at call
    /// time; later state changes are NOT reflected.
    pub fn from_catalogue(
        cat: &Catalogue,
        disk_quota_bytes: Option<u64>,
        connectivity: ConnectivityState,
    ) -> Result<Self> {
        // Single SQL aggregate -- not 10 separate counts. See
        // [`Catalogue::status_aggregate`] for the rationale (atomic
        // snapshot of counts + bytes + pinned in one read instead
        // of a sequence of independent queries that could observe a
        // row moving between statuses).
        let (cached_bytes, pinned_count, status_counts) = cat.status_aggregate()?;
        let over_quota = disk_quota_bytes.map(|q| cached_bytes > q).unwrap_or(false);
        Ok(Snapshot {
            workspace_id: cat.workspace_id(),
            cached_bytes,
            disk_quota_bytes,
            pinned_count,
            status_counts,
            connectivity: connectivity.into(),
            over_quota,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalogue::{FileRecord, SyncStatus};
    use std::path::PathBuf;
    use uuid::Uuid;

    fn rec(name: &str, status: SyncStatus, size: u64, pinned: bool) -> FileRecord {
        let now = chrono::Utc::now();
        FileRecord {
            remote_file_id: Uuid::new_v4(),
            remote_version_id: Uuid::new_v4(),
            local_path: PathBuf::from(format!("/tmp/{name}")),
            size_bytes: size,
            content_hash: [1u8; 32],
            status,
            pinned,
            updated_at: now,
            last_accessed_at: now,
        }
    }

    #[test]
    fn snapshot_counts_match_catalogue() {
        let tmp = tempfile::tempdir().unwrap();
        let ws = Uuid::new_v4();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), ws).unwrap();
        cat.upsert(&rec("a.bin", SyncStatus::UpToDate, 100, false))
            .unwrap();
        cat.upsert(&rec("b.bin", SyncStatus::UpToDate, 200, true))
            .unwrap();
        cat.upsert(&rec("c.bin", SyncStatus::LocalDirty, 50, false))
            .unwrap();
        cat.upsert(&rec("d.bin", SyncStatus::Conflict, 75, false))
            .unwrap();
        cat.upsert(&rec("e.bin", SyncStatus::Evicted, 0, false))
            .unwrap();

        let snap = Snapshot::from_catalogue(&cat, Some(1000), ConnectivityState::Online).unwrap();
        assert_eq!(snap.workspace_id, ws);
        assert_eq!(snap.cached_bytes, 425); // 100 + 200 + 50 + 75 (Evicted excluded)
        assert_eq!(snap.pinned_count, 1);
        assert_eq!(snap.status_counts.up_to_date, 2);
        assert_eq!(snap.status_counts.local_dirty, 1);
        assert_eq!(snap.status_counts.conflict, 1);
        assert_eq!(snap.status_counts.evicted, 1);
        assert_eq!(snap.status_counts.total(), 5);
        assert_eq!(snap.status_counts.pending(), 2);
        assert!(!snap.over_quota);
        assert_eq!(snap.connectivity, ConnectivityStateOwned::Online);
    }

    #[test]
    fn over_quota_flag_when_cached_exceeds_quota() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        cat.upsert(&rec("big.bin", SyncStatus::UpToDate, 10_000, false))
            .unwrap();
        let snap = Snapshot::from_catalogue(&cat, Some(5_000), ConnectivityState::Online).unwrap();
        assert!(snap.over_quota);
    }

    #[test]
    fn unbounded_quota_never_over() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        cat.upsert(&rec("huge.bin", SyncStatus::UpToDate, u64::MAX / 2, false))
            .unwrap();
        let snap = Snapshot::from_catalogue(&cat, None, ConnectivityState::Online).unwrap();
        assert!(!snap.over_quota);
    }
}
