//! Cross-workspace tray-state aggregator.
//!
//! The tray icon shows a single state for the whole app. We derive
//! it by reducing every workspace's [`SyncHealth`] into one icon
//! (priority order: Error → Conflict → Syncing → Idle → Stopped).
//! Counts are aggregated for tooltip / badge text.

use serde::{Deserialize, Serialize};

use crate::state::{SyncHealth, WorkspaceState};

/// Aggregated tray view across all registered workspaces. The
/// frontend renders this directly — it should never need to look at
/// per-workspace [`WorkspaceState`] to decide the tray icon.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct TrayState {
    pub health: SyncHealth,
    pub total_pending: u64,
    pub total_conflicts: u64,
    pub workspaces: usize,
    pub workspaces_running: usize,
    /// First error message across any errored workspace, surfaced
    /// in the tray tooltip. We pick the first one rather than
    /// concatenating because a tooltip is single-line; the
    /// per-workspace error remains accessible via [`WorkspaceState::last_error`].
    pub first_error: Option<String>,
}

impl TrayState {
    /// Builds the tray state from the current per-workspace
    /// snapshot. Pure function so it's straightforward to unit-test
    /// against fabricated inputs without spinning up an engine.
    pub fn derive(workspaces: &[WorkspaceState]) -> Self {
        if workspaces.is_empty() {
            return Self {
                health: SyncHealth::Stopped,
                total_pending: 0,
                total_conflicts: 0,
                workspaces: 0,
                workspaces_running: 0,
                first_error: None,
            };
        }
        let mut total_pending: u64 = 0;
        let mut total_conflicts: u64 = 0;
        let mut workspaces_running = 0;
        let mut first_error: Option<String> = None;

        // Track the highest-priority health value seen so far.
        // Priority order is the order Error > Conflict > Syncing
        // > Idle > Starting > Stopped — this matches what users
        // expect on a tray icon (alert states surface over success
        // states even if only one workspace is in trouble).
        let mut tray_health = SyncHealth::Stopped;
        for ws in workspaces {
            total_pending = total_pending.saturating_add(ws.summary.pending());
            total_conflicts = total_conflicts.saturating_add(ws.summary.conflict);
            if ws.health.is_running() {
                workspaces_running += 1;
            }
            if matches!(ws.health, SyncHealth::Error) && first_error.is_none() {
                first_error = ws.last_error.clone();
            }
            tray_health = max_health(tray_health, ws.health);
        }
        Self {
            health: tray_health,
            total_pending,
            total_conflicts,
            workspaces: workspaces.len(),
            workspaces_running,
            first_error,
        }
    }
}

/// Returns the higher-priority of two [`SyncHealth`] values. Pure
/// function, broken out so the aggregation order is testable in
/// isolation.
///
/// Priority (high → low):
///
/// 1. `Error`     — at least one workspace can't reach the server.
/// 2. `Conflict`  — at least one file needs user resolution.
/// 3. `Syncing`   — at least one transfer is in flight.
/// 4. `Idle`      — caught up, live socket connected.
/// 5. `Starting`  — catch-up in progress; not yet healthy.
/// 6. `Stopped`   — workspace registered, no tasks running.
fn max_health(a: SyncHealth, b: SyncHealth) -> SyncHealth {
    fn rank(h: SyncHealth) -> u8 {
        match h {
            SyncHealth::Error => 5,
            SyncHealth::Conflict => 4,
            SyncHealth::Syncing => 3,
            SyncHealth::Idle => 2,
            SyncHealth::Starting => 1,
            SyncHealth::Stopped => 0,
        }
    }
    if rank(a) >= rank(b) {
        a
    } else {
        b
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Summary;
    use chrono::Utc;
    use std::path::PathBuf;
    use uuid::Uuid;

    fn ws(health: SyncHealth, conflicts: u64, in_flight: u64) -> WorkspaceState {
        let mut summary = Summary::default();
        for _ in 0..conflicts {
            summary.accumulate(zk_sync_engine::SyncStatus::Conflict, 0);
        }
        for _ in 0..in_flight {
            summary.accumulate(zk_sync_engine::SyncStatus::InFlight, 0);
        }
        WorkspaceState {
            workspace_id: Uuid::new_v4(),
            label: "ws".into(),
            root: PathBuf::from("/tmp/ws"),
            health,
            summary,
            last_error: if matches!(health, SyncHealth::Error) {
                Some("boom".into())
            } else {
                None
            },
            last_updated: Utc::now(),
        }
    }

    #[test]
    fn empty_app_renders_stopped() {
        let t = TrayState::derive(&[]);
        assert_eq!(t.health, SyncHealth::Stopped);
        assert_eq!(t.total_pending, 0);
        assert_eq!(t.total_conflicts, 0);
        assert_eq!(t.workspaces, 0);
        assert_eq!(t.workspaces_running, 0);
        assert!(t.first_error.is_none());
    }

    #[test]
    fn error_wins_over_conflict() {
        let t = TrayState::derive(&[ws(SyncHealth::Conflict, 3, 0), ws(SyncHealth::Error, 0, 0)]);
        assert_eq!(t.health, SyncHealth::Error);
        assert_eq!(t.total_conflicts, 3);
        assert_eq!(t.first_error.as_deref(), Some("boom"));
    }

    #[test]
    fn conflict_wins_over_syncing() {
        let t = TrayState::derive(&[
            ws(SyncHealth::Syncing, 0, 2),
            ws(SyncHealth::Conflict, 1, 0),
        ]);
        assert_eq!(t.health, SyncHealth::Conflict);
        assert_eq!(t.total_conflicts, 1);
        // Both contribute to pending — in-flight + conflict.
        assert_eq!(t.total_pending, 3);
    }

    #[test]
    fn idle_with_no_pending_shows_idle() {
        let t = TrayState::derive(&[ws(SyncHealth::Idle, 0, 0)]);
        assert_eq!(t.health, SyncHealth::Idle);
        assert_eq!(t.total_pending, 0);
        assert_eq!(t.workspaces_running, 1);
    }

    #[test]
    fn running_count_excludes_stopped_workspaces() {
        let t = TrayState::derive(&[
            ws(SyncHealth::Idle, 0, 0),
            ws(SyncHealth::Stopped, 0, 0),
            ws(SyncHealth::Syncing, 0, 1),
        ]);
        assert_eq!(t.workspaces, 3);
        assert_eq!(t.workspaces_running, 2);
    }

    #[test]
    fn syncing_dominates_idle() {
        let t = TrayState::derive(&[
            ws(SyncHealth::Idle, 0, 0),
            ws(SyncHealth::Syncing, 0, 1),
            ws(SyncHealth::Idle, 0, 0),
        ]);
        assert_eq!(t.health, SyncHealth::Syncing);
    }
}
