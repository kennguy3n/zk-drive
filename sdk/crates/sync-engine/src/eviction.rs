//! LRU cache eviction for the offline file cache.
//!
//! ZK Drive's desktop sync agent treats the local filesystem as a
//! tiered cache of the remote workspace. The catalogue holds rows
//! for every file the user might want to read offline, but the
//! local disk only holds bytes for a subset of them; the rest are
//! either pinned (kept forever) or non-pinned (kept until the disk
//! quota is reached, then evicted under an LRU policy). When the
//! user opens an evicted file the downloader (PR6) fetches it
//! back from the server and the engine flips the row from
//! [`SyncStatus::Evicted`] back to [`SyncStatus::UpToDate`].
//!
//! This module owns the "decide which rows to evict" half of that
//! lifecycle. The "fetch back when accessed" half lives in the
//! downloader. The two halves communicate exclusively through the
//! catalogue: an evicted row has its on-disk file unlinked and its
//! catalogue status set to `Evicted`, but the row itself stays in
//! the catalogue so the downloader can resurrect it later.
//!
//! ## Safety invariants
//!
//! * **Pinned rows are NEVER evicted.** The eviction candidate
//!   query at [`Catalogue::eviction_candidates`] filters
//!   `pinned = 0` at the SQL layer, so we don't even materialise a
//!   pinned [`FileRecord`] in this module.
//! * **Only `UpToDate` rows are evicted.** A `LocalDirty` row holds
//!   bytes the server hasn't seen; evicting it would silently lose
//!   the user's unsaved changes. A `RemoteDirty` row holds bytes
//!   the engine is about to overwrite anyway, but the row's
//!   `local_path` is the *target* of the download, and evicting it
//!   would leave the catalogue pointing at a path that doesn't
//!   exist on disk. A `Conflict` row needs human resolution.
//!   `InFlight` is racing against the transfer loop. `Evicted` is
//!   already a tombstone.
//! * **Zero-byte rows are skipped.** A zero-byte file reclaims no
//!   space, and the placeholder paths from the catch-up flow are
//!   typically zero-byte; evicting them is busywork.
//! * **The local file is unlinked BEFORE the catalogue status
//!   flips.** If the catalogue update succeeds but the unlink
//!   fails, the next eviction pass picks the row up again (it's
//!   still `UpToDate` from the catalogue's perspective) and tries
//!   again. The reverse order would leave a phantom file on disk
//!   that the engine no longer knows about.
//!
//! ## Quota policy
//!
//! The evictor consults [`Catalogue::total_cached_bytes`] to learn
//! the current footprint and evicts oldest-access-first until the
//! footprint is back under the configured quota. The quota itself
//! is sourced from [`crate::EngineConfig::disk_quota_bytes`]; when
//! unset, no eviction runs (the engine treats the disk as
//! unbounded, which is the right default for development).

use std::path::Path;

use tracing::{info, warn};

use crate::catalogue::{Catalogue, SyncStatus};
use crate::Result;

/// Result of one [`evict_to_quota`] pass.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct EvictionReport {
    /// Number of catalogue rows transitioned to [`SyncStatus::Evicted`].
    pub evicted_count: u64,
    /// On-disk bytes reclaimed (sum of `size_bytes` for every
    /// evicted row). The figure is the catalogue's view, not the
    /// filesystem's; in the rare case the two diverge (e.g. a row
    /// whose on-disk file was hand-deleted out-of-band) the
    /// reported count still represents quota relief because the
    /// catalogue stops counting the row against quota.
    pub bytes_reclaimed: u64,
    /// Final cached footprint reported by the catalogue after the
    /// pass. Surfaced so callers can log "evicted X to keep cache
    /// under Y" without re-querying.
    pub final_cached_bytes: u64,
    /// True if the evictor exhausted its candidate pool before the
    /// quota was satisfied. Indicates the workspace has more pinned
    /// bytes than the quota; the operator should either raise the
    /// quota or unpin some rows.
    pub quota_unreachable: bool,
}

/// Why an eviction pass ran. Surfaced in logs so an operator
/// inspecting the pass can distinguish a CLI-initiated sweep from
/// the engine's autonomous quota-relief loop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EvictionTrigger {
    /// Operator invoked `zk-sync evict` from the CLI.
    Manual,
    /// Engine's background loop noticed `total_cached_bytes >
    /// quota` and triggered eviction.
    QuotaExceeded,
}

impl EvictionTrigger {
    fn as_str(self) -> &'static str {
        match self {
            EvictionTrigger::Manual => "manual",
            EvictionTrigger::QuotaExceeded => "quota_exceeded",
        }
    }
}

/// Page size for the candidate scan. Bounded so we don't load the
/// whole catalogue into memory on a single pass; the eviction loop
/// iterates pages until the quota is met.
const CANDIDATE_PAGE_SIZE: usize = 256;

/// Bring `cat`'s cached footprint at or below `quota_bytes` by
/// evicting non-pinned `UpToDate` rows in LRU order. Returns an
/// [`EvictionReport`] summarising the pass.
///
/// The function takes a `root: &Path` only for log context (every
/// row's `local_path` is already absolute); it is NOT used to
/// resolve relative paths. The caller is expected to pass
/// [`crate::EngineConfig::root`] but any path will do.
///
/// `quota_bytes == 0` is a legal value: it requests "evict every
/// non-pinned UpToDate row, retain only pinned content". This is
/// the right semantic for "the user wants to free disk now" and
/// for a future "go offline only on pinned content" tray-UI option.
///
/// `trigger` is recorded in the structured log line at info level
/// so an operator inspecting `journalctl -u zk-sync` can see why
/// the pass ran.
pub fn evict_to_quota(
    cat: &mut Catalogue,
    root: &Path,
    quota_bytes: u64,
    trigger: EvictionTrigger,
) -> Result<EvictionReport> {
    let mut report = EvictionReport::default();
    let mut current = cat.total_cached_bytes()?;
    let initial = current;
    if current <= quota_bytes {
        report.final_cached_bytes = current;
        info!(
            trigger = trigger.as_str(),
            root = %root.display(),
            current = current,
            quota = quota_bytes,
            "eviction skipped: footprint already within quota",
        );
        return Ok(report);
    }

    loop {
        let candidates = cat.eviction_candidates(CANDIDATE_PAGE_SIZE)?;
        if candidates.is_empty() {
            // Every non-pinned UpToDate row was already considered
            // (and skipped or evicted on prior iterations of this
            // outer loop); if we're still over quota the workspace
            // is pinned-heavy.
            report.quota_unreachable = current > quota_bytes;
            break;
        }
        let mut evicted_any = false;
        for rec in candidates {
            if current <= quota_bytes {
                break;
            }
            // Unlink first, catalogue second. See the module-level
            // "Safety invariants" comment for the ordering rationale.
            match std::fs::remove_file(&rec.local_path) {
                Ok(()) => {}
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                    // File was already gone (user deleted it
                    // out-of-band, or a previous eviction succeeded
                    // at the FS layer but crashed before updating
                    // the catalogue). Treat as success -- the
                    // desired end state is "no file on disk + row
                    // marked Evicted" and we just achieved half of
                    // that.
                    warn!(
                        path = %rec.local_path.display(),
                        file_id = %rec.remote_file_id,
                        "eviction: file already missing; will mark catalogue row Evicted anyway",
                    );
                }
                Err(e) => {
                    // ENOSPC at unlink (unlikely but possible on
                    // some filesystems), EPERM, EBUSY (mmap),
                    // EIO -- anything other than NotFound. Skip
                    // this row and let the next pass try again. We
                    // do NOT propagate the error because evicting
                    // is a best-effort cache hint, not a sync
                    // correctness operation: the catalogue stays
                    // correct (row still UpToDate), the file stays
                    // on disk, and the next pass might succeed.
                    warn!(
                        path = %rec.local_path.display(),
                        file_id = %rec.remote_file_id,
                        err = %e,
                        "eviction: unlink failed; skipping row",
                    );
                    continue;
                }
            }
            cat.set_status(rec.remote_file_id, SyncStatus::Evicted)?;
            report.evicted_count = report.evicted_count.saturating_add(1);
            report.bytes_reclaimed = report.bytes_reclaimed.saturating_add(rec.size_bytes);
            // Update local total without re-querying: each evicted
            // row removes its size_bytes from `total_cached_bytes`
            // (Evicted is excluded from the sum at the SQL layer).
            current = current.saturating_sub(rec.size_bytes);
            evicted_any = true;
        }
        if !evicted_any || current <= quota_bytes {
            break;
        }
    }

    // Final query to re-anchor the count: between our local
    // accounting and any concurrent catalogue mutation by the
    // engine loop (e.g. an upload finishing and flipping a row to
    // UpToDate), the SQL ground truth is what callers should see.
    let final_bytes = cat.total_cached_bytes()?;
    report.final_cached_bytes = final_bytes;
    info!(
        trigger = trigger.as_str(),
        root = %root.display(),
        initial = initial,
        evicted = report.evicted_count,
        reclaimed = report.bytes_reclaimed,
        final_bytes = final_bytes,
        quota = quota_bytes,
        unreachable = report.quota_unreachable,
        "eviction pass complete",
    );
    Ok(report)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalogue::FileRecord;
    use chrono::Duration;
    use tempfile::TempDir;
    use uuid::Uuid;

    /// Builds a real on-disk file of `size_bytes` bytes under `root`
    /// with a corresponding `FileRecord` whose `last_accessed_at`
    /// is `age_seconds` before the test's reference instant. Using
    /// real files (not stubs) is essential here: the evictor's
    /// correctness hinges on `std::fs::remove_file` actually
    /// deleting the file, and any pure-catalogue test would fail to
    /// catch a bug where the unlink and the catalogue update are
    /// in the wrong order.
    fn make_file(
        root: &Path,
        name: &str,
        size_bytes: u64,
        age_seconds: i64,
        pinned: bool,
        status: SyncStatus,
        now: chrono::DateTime<chrono::Utc>,
    ) -> FileRecord {
        let path = root.join(name);
        let data = vec![b'x'; size_bytes as usize];
        std::fs::write(&path, &data).unwrap();
        FileRecord {
            remote_file_id: Uuid::new_v4(),
            remote_version_id: Uuid::new_v4(),
            local_path: path,
            size_bytes,
            content_hash: [1u8; 32],
            status,
            pinned,
            updated_at: now - Duration::seconds(age_seconds),
            last_accessed_at: now - Duration::seconds(age_seconds),
        }
    }

    fn open(tmp: &TempDir) -> Catalogue {
        Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap()
    }

    #[test]
    fn evicts_oldest_first_until_under_quota() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();

        // Three rows, 100 bytes each, total 300. Quota = 150.
        // Expect: the two oldest evicted (200 bytes reclaimed),
        // newest survives (100 bytes remain), final = 100 <= 150.
        let oldest = make_file(
            tmp.path(),
            "old.bin",
            100,
            300,
            false,
            SyncStatus::UpToDate,
            now,
        );
        let middle = make_file(
            tmp.path(),
            "mid.bin",
            100,
            200,
            false,
            SyncStatus::UpToDate,
            now,
        );
        let newest = make_file(
            tmp.path(),
            "new.bin",
            100,
            100,
            false,
            SyncStatus::UpToDate,
            now,
        );
        cat.upsert(&oldest).unwrap();
        cat.upsert(&middle).unwrap();
        cat.upsert(&newest).unwrap();
        assert_eq!(cat.total_cached_bytes().unwrap(), 300);

        let report = evict_to_quota(&mut cat, tmp.path(), 150, EvictionTrigger::Manual).unwrap();
        assert_eq!(report.evicted_count, 2);
        assert_eq!(report.bytes_reclaimed, 200);
        assert_eq!(report.final_cached_bytes, 100);
        assert!(!report.quota_unreachable);

        // On disk: oldest + middle gone, newest survives.
        assert!(!oldest.local_path.exists(), "oldest must be unlinked");
        assert!(!middle.local_path.exists(), "middle must be unlinked");
        assert!(newest.local_path.exists(), "newest must survive");

        // Catalogue: oldest + middle marked Evicted, newest still UpToDate.
        assert_eq!(
            cat.get(oldest.remote_file_id).unwrap().unwrap().status,
            SyncStatus::Evicted
        );
        assert_eq!(
            cat.get(middle.remote_file_id).unwrap().unwrap().status,
            SyncStatus::Evicted
        );
        assert_eq!(
            cat.get(newest.remote_file_id).unwrap().unwrap().status,
            SyncStatus::UpToDate
        );
    }

    #[test]
    fn skips_pinned_rows_even_if_oldest() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();

        // Pinned row is the oldest; evicting it would be a bug.
        // Quota = 100, total = 200; evict the unpinned 100B row,
        // keep the pinned 100B row.
        let pinned = make_file(
            tmp.path(),
            "pin.bin",
            100,
            999,
            true,
            SyncStatus::UpToDate,
            now,
        );
        let recent = make_file(
            tmp.path(),
            "rec.bin",
            100,
            50,
            false,
            SyncStatus::UpToDate,
            now,
        );
        cat.upsert(&pinned).unwrap();
        cat.upsert(&recent).unwrap();

        let report = evict_to_quota(&mut cat, tmp.path(), 100, EvictionTrigger::Manual).unwrap();
        assert_eq!(report.evicted_count, 1);
        assert!(
            pinned.local_path.exists(),
            "pinned row must NEVER be evicted"
        );
        assert!(!recent.local_path.exists());
        assert_eq!(
            cat.get(pinned.remote_file_id).unwrap().unwrap().status,
            SyncStatus::UpToDate
        );
    }

    #[test]
    fn skips_non_uptodate_rows() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();

        // LocalDirty has unsaved user changes -- never evict.
        let dirty = make_file(
            tmp.path(),
            "dirty.bin",
            100,
            999,
            false,
            SyncStatus::LocalDirty,
            now,
        );
        // RemoteDirty has a queued download targeting this path --
        // never evict.
        let r_dirty = make_file(
            tmp.path(),
            "rdirty.bin",
            100,
            999,
            false,
            SyncStatus::RemoteDirty,
            now,
        );
        let upto = make_file(
            tmp.path(),
            "u.bin",
            100,
            50,
            false,
            SyncStatus::UpToDate,
            now,
        );
        cat.upsert(&dirty).unwrap();
        cat.upsert(&r_dirty).unwrap();
        cat.upsert(&upto).unwrap();

        let report = evict_to_quota(&mut cat, tmp.path(), 0, EvictionTrigger::Manual).unwrap();
        // Even with quota=0 (max-aggressive), only the UpToDate
        // row evicts; the two pending rows survive.
        assert_eq!(report.evicted_count, 1);
        assert!(dirty.local_path.exists());
        assert!(r_dirty.local_path.exists());
        assert!(!upto.local_path.exists());
    }

    #[test]
    fn reports_unreachable_when_pinned_exceeds_quota() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();
        // 300B of pinned content; quota = 100. Nothing to evict;
        // unreachable=true so the operator knows to unpin.
        for i in 0..3 {
            let r = make_file(
                tmp.path(),
                &format!("p{i}.bin"),
                100,
                100 + i,
                true,
                SyncStatus::UpToDate,
                now,
            );
            cat.upsert(&r).unwrap();
        }
        let report =
            evict_to_quota(&mut cat, tmp.path(), 100, EvictionTrigger::QuotaExceeded).unwrap();
        assert_eq!(report.evicted_count, 0);
        assert!(report.quota_unreachable);
        assert_eq!(report.final_cached_bytes, 300);
    }

    #[test]
    fn missing_file_still_marks_catalogue_evicted() {
        // The contract: if the on-disk file is already gone (user
        // hand-deleted, prior pass crashed mid-eviction), the
        // catalogue must still converge to Evicted so the row
        // doesn't keep re-appearing in eviction_candidates.
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();
        let r = make_file(
            tmp.path(),
            "ghost.bin",
            100,
            999,
            false,
            SyncStatus::UpToDate,
            now,
        );
        cat.upsert(&r).unwrap();
        std::fs::remove_file(&r.local_path).unwrap();
        // Quota = 0 forces eviction. The unlink will return
        // NotFound; we expect the row to still be marked Evicted.
        let report = evict_to_quota(&mut cat, tmp.path(), 0, EvictionTrigger::Manual).unwrap();
        assert_eq!(report.evicted_count, 1);
        assert_eq!(
            cat.get(r.remote_file_id).unwrap().unwrap().status,
            SyncStatus::Evicted
        );
    }

    #[test]
    fn quota_zero_evicts_everything_unpinned() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();
        for i in 0..5 {
            let r = make_file(
                tmp.path(),
                &format!("f{i}.bin"),
                50,
                100 + i,
                false,
                SyncStatus::UpToDate,
                now,
            );
            cat.upsert(&r).unwrap();
        }
        let report = evict_to_quota(&mut cat, tmp.path(), 0, EvictionTrigger::Manual).unwrap();
        assert_eq!(report.evicted_count, 5);
        assert_eq!(report.bytes_reclaimed, 250);
        assert_eq!(report.final_cached_bytes, 0);
    }

    #[test]
    fn no_op_when_under_quota() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();
        let r = make_file(
            tmp.path(),
            "small.bin",
            50,
            999,
            false,
            SyncStatus::UpToDate,
            now,
        );
        cat.upsert(&r).unwrap();
        let report =
            evict_to_quota(&mut cat, tmp.path(), 1024, EvictionTrigger::QuotaExceeded).unwrap();
        assert_eq!(report.evicted_count, 0);
        assert!(r.local_path.exists());
    }

    #[test]
    fn paginates_over_large_workspace() {
        // Smoke test that the loop terminates and produces a
        // monotonically-shrinking footprint when the catalogue
        // has more candidates than CANDIDATE_PAGE_SIZE.
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = open(&tmp);
        let now = chrono::Utc::now();
        let count = CANDIDATE_PAGE_SIZE + 50;
        for i in 0..count {
            let r = make_file(
                tmp.path(),
                &format!("f{i}.bin"),
                10,
                10000 - i as i64,
                false,
                SyncStatus::UpToDate,
                now,
            );
            cat.upsert(&r).unwrap();
        }
        // Quota=200 means we should keep 20 rows and evict the rest.
        let report = evict_to_quota(&mut cat, tmp.path(), 200, EvictionTrigger::Manual).unwrap();
        assert!(
            report.final_cached_bytes <= 200,
            "final={}",
            report.final_cached_bytes
        );
        assert_eq!(report.evicted_count, (count - 20) as u64);
    }
}
