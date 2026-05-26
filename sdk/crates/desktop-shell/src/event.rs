//! Event bus: the shell pushes [`ShellEvent`]s to subscribers
//! whenever workspace state changes.
//!
//! The default sink is a tokio `broadcast::Sender`, which gives N
//! independent receivers (one per Tauri window, one per IPC client,
//! …) and drops the oldest message on slow consumers. Hosts that
//! want a custom delivery story (e.g. push to a websocket from a
//! Tauri command) can implement [`EventSink`] themselves.

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::state::SyncHealth;
use crate::Summary;
use crate::TrayState;

/// One observable event the shell can emit. Each variant is shaped
/// for direct JSON serialisation onto a Tauri / Electron channel.
///
/// The variant set is **append-only**. Frontends switch on
/// `#[serde(tag = "type", content = "data")]` and a forward-compat
/// unknown branch; renaming a variant would be a wire-format break
/// for already-deployed Tauri builds.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", content = "data", rename_all = "snake_case")]
pub enum ShellEvent {
    /// A workspace was added to the registry. Frontends use this to
    /// rebuild their workspace-list view; the [`Summary`] starts at
    /// the default zero state so progress bars render correctly
    /// before the first health-poll tick.
    WorkspaceAdded { workspace_id: Uuid, label: String },
    /// A workspace was removed from the registry. The frontend
    /// should drop any per-workspace state immediately — the
    /// catalogue file is **not** deleted from disk; that's a
    /// separate `RemoveLocalCache` admin action so a user can
    /// recover sync state after an accidental "remove" click.
    WorkspaceRemoved { workspace_id: Uuid },
    /// A workspace transitioned between [`SyncHealth`] values.
    /// Frontends use this to drive the per-workspace badge and to
    /// recompute the tray state.
    HealthChanged {
        workspace_id: Uuid,
        health: SyncHealth,
        /// Populated only when transitioning into
        /// [`SyncHealth::Error`]. Cleared on the next non-error
        /// transition.
        reason: Option<String>,
    },
    /// A fresh catalogue summary was sampled for a workspace. The
    /// shell emits one every [`crate::app::HEALTH_POLL_INTERVAL`]
    /// after a change is detected; identical summaries are
    /// suppressed so the frontend doesn't see redundant updates.
    SummaryChanged {
        workspace_id: Uuid,
        summary: Summary,
    },
    /// The cross-workspace tray aggregate changed. The frontend
    /// can subscribe to *only* this variant and ignore the per-
    /// workspace stream if it just renders a tray icon.
    TrayChanged { tray: TrayState },
    /// A workspace task panicked or returned an error. Pushed
    /// before the `HealthChanged{Error}` event so a frontend log
    /// has the underlying reason captured in chronological order.
    TaskFailed {
        workspace_id: Uuid,
        task: TaskKind,
        message: String,
    },
}

/// Which background task a [`ShellEvent::TaskFailed`] describes.
/// Kept as a typed enum rather than a free string so a frontend can
/// switch on it without parsing log lines.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskKind {
    Engine,
    Poller,
    Watcher,
    HealthLoop,
}

/// Receiving end of the event bus. Hosts implement this once and
/// hand the implementation to [`crate::App::with_sink`].
#[async_trait]
pub trait EventSink: Send + Sync {
    /// Deliver one event. The shell never awaits the call — sinks
    /// that need to do async work should spawn a task internally
    /// and return immediately, so the event loop is never blocked
    /// by a slow consumer.
    async fn emit(&self, event: ShellEvent);
}

/// Default sink built on a `tokio::sync::broadcast` channel.
///
/// Capacity is intentionally small (32) — the shell is intended to
/// fan an event to a handful of receivers (one per Tauri window,
/// one for the JSON-RPC client, one for tests). A consumer that
/// can't keep up drops the oldest message; the shell's invariant
/// is that the *latest* state is always reachable through the
/// `WorkspaceState::Get` command, so a dropped event never causes
/// the GUI to render permanently stale data.
pub struct BroadcastSink {
    tx: tokio::sync::broadcast::Sender<ShellEvent>,
}

impl BroadcastSink {
    pub fn new() -> Self {
        let (tx, _) = tokio::sync::broadcast::channel(32);
        Self { tx }
    }

    pub fn with_capacity(capacity: usize) -> Self {
        let (tx, _) = tokio::sync::broadcast::channel(capacity);
        Self { tx }
    }

    /// Subscribe to events. Each call returns a fresh receiver so
    /// multi-window frontends don't share a single channel.
    pub fn subscribe(&self) -> tokio::sync::broadcast::Receiver<ShellEvent> {
        self.tx.subscribe()
    }

    /// Returns the number of currently-active receivers. Useful in
    /// tests to assert nothing is leaking subscriptions.
    pub fn receiver_count(&self) -> usize {
        self.tx.receiver_count()
    }
}

impl Default for BroadcastSink {
    fn default() -> Self {
        Self::new()
    }
}

#[async_trait]
impl EventSink for BroadcastSink {
    async fn emit(&self, event: ShellEvent) {
        // `send` returns `Err` iff there are no live receivers —
        // perfectly fine, just drop the event silently. The
        // `WorkspaceState::Get` command is the source of truth
        // when no one is listening.
        let _ = self.tx.send(event);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn broadcast_sink_delivers_to_every_receiver() {
        let sink = BroadcastSink::new();
        let mut r1 = sink.subscribe();
        let mut r2 = sink.subscribe();
        let ev = ShellEvent::WorkspaceRemoved {
            workspace_id: Uuid::nil(),
        };
        sink.emit(ev.clone()).await;
        assert_eq!(r1.recv().await.unwrap(), ev);
        assert_eq!(r2.recv().await.unwrap(), ev);
    }

    #[tokio::test]
    async fn broadcast_sink_with_no_subscribers_is_a_noop() {
        let sink = BroadcastSink::new();
        // No subscribe() call -- the send below must not panic or
        // hang. The contract is "best-effort delivery, latest state
        // available via Get".
        sink.emit(ShellEvent::TrayChanged {
            tray: TrayState {
                health: SyncHealth::Idle,
                total_pending: 0,
                total_conflicts: 0,
                workspaces: 0,
                workspaces_running: 0,
                first_error: None,
            },
        })
        .await;
        assert_eq!(sink.receiver_count(), 0);
    }

    #[test]
    fn shell_event_serialises_as_tagged_json() {
        // Pin the wire format -- the Tauri main / Electron preload
        // pattern-matches on `type`, so renaming a tag breaks
        // already-deployed builds.
        let ev = ShellEvent::HealthChanged {
            workspace_id: Uuid::nil(),
            health: SyncHealth::Idle,
            reason: None,
        };
        let s = serde_json::to_string(&ev).unwrap();
        assert!(s.contains("\"type\":\"health_changed\""));
        assert!(s.contains("\"health\":\"idle\""));
    }
}
