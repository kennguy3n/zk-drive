//! Catalogue summary — a frontend-friendly count-by-status snapshot.
//!
//! The summary is the unit of progress the shell broadcasts on every
//! poll. Frontends use it both for the tray badge counts and for
//! per-workspace progress bars.

use serde::{Deserialize, Serialize};
use zk_sync_engine::SyncStatus;

/// Aggregate counts over the catalogue's `files` table. All fields
/// are `u64` because the catalogue's `size_bytes` column is `i64`
/// (SQLite) but never negative in practice — the engine treats it
/// as an unsigned size on every write path.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
pub struct Summary {
    pub total_files: u64,
    pub total_bytes: u64,
    pub up_to_date: u64,
    pub local_dirty: u64,
    pub local_deleted: u64,
    pub remote_dirty: u64,
    pub remote_deleted: u64,
    pub conflict: u64,
    pub in_flight: u64,
    pub evicted: u64,
    /// Last-applied changefeed cursor for the workspace. Frontends
    /// surface this in the "Advanced" / debug panel; not used for
    /// any user-facing decision.
    pub cursor: i64,
}

impl Summary {
    /// Returns the number of files that still have work pending —
    /// anything that is neither [`SyncStatus::UpToDate`] (already
    /// settled) nor [`SyncStatus::Evicted`] (deliberately dropped
    /// from the local cache, no work to do until a remote change
    /// re-enters them into the work set). Frontends use this as
    /// the "still syncing" tray-icon test and as the progress-bar
    /// denominator. The exclusion of `Evicted` is pinned by
    /// [`pending_excludes_up_to_date_and_evicted`](tests::pending_excludes_up_to_date_and_evicted)
    /// below.
    pub fn pending(self) -> u64 {
        self.local_dirty
            + self.local_deleted
            + self.remote_dirty
            + self.remote_deleted
            + self.conflict
            + self.in_flight
    }

    /// Fold one [`zk_sync_engine::FileRecord`]-equivalent row into
    /// the summary. Kept as a free function-ish method so the
    /// caller (the catalogue poller) can iterate `list_all()` once
    /// rather than reading every row twice.
    pub fn accumulate(&mut self, status: SyncStatus, size_bytes: u64) {
        self.total_files += 1;
        self.total_bytes = self.total_bytes.saturating_add(size_bytes);
        match status {
            SyncStatus::UpToDate => self.up_to_date += 1,
            SyncStatus::LocalDirty => self.local_dirty += 1,
            SyncStatus::LocalDeleted => self.local_deleted += 1,
            SyncStatus::RemoteDirty => self.remote_dirty += 1,
            SyncStatus::RemoteDeleted => self.remote_deleted += 1,
            SyncStatus::Conflict => self.conflict += 1,
            SyncStatus::InFlight => self.in_flight += 1,
            SyncStatus::Evicted => self.evicted += 1,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pending_excludes_up_to_date_and_evicted() {
        // Evicted rows don't need work — the offline-cache crate
        // dropped them deliberately. They re-enter the work set
        // only on a remote change.
        let mut s = Summary::default();
        s.accumulate(SyncStatus::UpToDate, 1_000);
        s.accumulate(SyncStatus::Evicted, 2_000);
        assert_eq!(s.pending(), 0);
        assert_eq!(s.total_files, 2);
    }

    #[test]
    fn pending_counts_every_pending_status() {
        let mut s = Summary::default();
        s.accumulate(SyncStatus::LocalDirty, 1);
        s.accumulate(SyncStatus::LocalDeleted, 1);
        s.accumulate(SyncStatus::RemoteDirty, 1);
        s.accumulate(SyncStatus::RemoteDeleted, 1);
        s.accumulate(SyncStatus::Conflict, 1);
        s.accumulate(SyncStatus::InFlight, 1);
        assert_eq!(s.pending(), 6);
        assert_eq!(s.total_files, 6);
    }

    #[test]
    fn total_bytes_saturates_rather_than_panics() {
        // The catalogue is supposed to clip absurd sizes but
        // saturating_add means a malformed row still doesn't crash
        // the summary loop. Pin the behaviour so a future refactor
        // can't accidentally swap in a wrapping add.
        let mut s = Summary::default();
        s.accumulate(SyncStatus::UpToDate, u64::MAX);
        s.accumulate(SyncStatus::UpToDate, 1);
        assert_eq!(s.total_bytes, u64::MAX);
    }
}
