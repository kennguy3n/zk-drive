//! Online/offline state tracked by the sync engine.
//!
//! The poller and the (future) uploader use this shared state to
//! decide whether to attempt network I/O at all: when the engine
//! is "offline" they back off cleanly rather than retrying every
//! request and burning the user's battery on a captive-portal Wi-Fi
//! or an ICE-blocked mobile hotspot. The state is observable from
//! the CLI's status command so the user sees "Offline — 3 pending
//! uploads" rather than a generic spinner.
//!
//! State transitions are driven by the request loop, not by an
//! out-of-band probe. We do NOT ping the network or poll
//! `/healthz`: the truthful signal is "did our last real request
//! succeed?", and a synthetic probe would just be a second signal
//! that could disagree with reality. Specifically:
//!
//!   * Any [`zk_sync_api::ApiError::Transport`] failure marks the
//!     state [`ConnectivityState::Offline`] because that's the
//!     reqwest variant for connect-refused / DNS-failed / TLS-
//!     handshake-aborted, all of which are network-layer issues.
//!   * Any successful request flips the state back to
//!     [`ConnectivityState::Online`].
//!   * HTTP errors (4xx / 5xx) leave the state unchanged: the
//!     server responded, so we ARE online; the request just
//!     failed for an application-level reason.
//!
//! The state itself is a shared atomic so the poller's `live` task
//! and a tray UI thread can both consult it without holding a
//! mutex.

use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::Arc;

/// Three-state online indicator.
///
/// `Unknown` is the initial state at engine startup, before the
/// first request has happened. We distinguish it from `Offline` so
/// the CLI status output can say "not yet polled" rather than
/// falsely claiming connectivity is down.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum ConnectivityState {
    Unknown = 0,
    Online = 1,
    Offline = 2,
}

impl ConnectivityState {
    fn from_u8(v: u8) -> Self {
        match v {
            1 => ConnectivityState::Online,
            2 => ConnectivityState::Offline,
            _ => ConnectivityState::Unknown,
        }
    }

    /// Human label for status output / log fields.
    pub fn as_str(self) -> &'static str {
        match self {
            ConnectivityState::Unknown => "unknown",
            ConnectivityState::Online => "online",
            ConnectivityState::Offline => "offline",
        }
    }
}

/// Shared connectivity flag.
///
/// Cloning is cheap (`Arc<AtomicU8>`); the poller, the uploader,
/// and the CLI all hold a clone.
#[derive(Debug, Clone, Default)]
pub struct OnlineState {
    inner: Arc<AtomicU8>,
}

impl OnlineState {
    pub fn new() -> Self {
        Self::default()
    }

    /// Read the current state. Cheap; relaxed ordering is fine
    /// because consumers only need eventual consistency (a stale
    /// read at worst causes one extra request attempt on the next
    /// poll loop iteration).
    pub fn get(&self) -> ConnectivityState {
        ConnectivityState::from_u8(self.inner.load(Ordering::Relaxed))
    }

    /// Mark the engine online. Called by the poller when any
    /// network request succeeds.
    pub fn mark_online(&self) {
        self.inner
            .store(ConnectivityState::Online as u8, Ordering::Relaxed);
    }

    /// Mark the engine offline. Called by the poller when a
    /// reqwest transport error fires (DNS, connect, TLS).
    pub fn mark_offline(&self) {
        self.inner
            .store(ConnectivityState::Offline as u8, Ordering::Relaxed);
    }

    /// True if the engine considers itself online. Convenience
    /// alias for `state == Online`; `Unknown` returns false because
    /// callers using this method are typically asking "should I
    /// attempt a request?", and the safe answer when we haven't
    /// yet probed the network is "yes, try" -- but the only caller
    /// pattern that uses `is_online()` for a *guard* is when we've
    /// already failed once, so `Unknown -> false` would be wrong.
    ///
    /// We resolve this by treating `Unknown` as "online enough to
    /// try"; it returns true. The signal is only meaningful once
    /// we've made at least one request, and the safer default for
    /// the first request is to attempt it.
    pub fn is_online(&self) -> bool {
        matches!(
            self.get(),
            ConnectivityState::Online | ConnectivityState::Unknown
        )
    }

    /// Inspect API failures and update the state. Returns the new
    /// state. Used by the poller's request site to maintain the
    /// flag without each call site re-encoding the transport-error
    /// taxonomy.
    pub fn record_api_result<T>(
        &self,
        res: &Result<T, zk_sync_api::ApiError>,
    ) -> ConnectivityState {
        match res {
            Ok(_) => {
                self.mark_online();
            }
            Err(zk_sync_api::ApiError::Transport(_)) => {
                self.mark_offline();
            }
            Err(zk_sync_api::ApiError::WebSocket(_)) => {
                // tokio-tungstenite surfaces both transport errors
                // (no DNS, no TCP) and protocol errors (bad upgrade
                // header) through the same WebSocket(...) variant.
                // We can't distinguish here without parsing the
                // error string, so we conservatively mark offline:
                // a real protocol error would persist past a
                // restart, at which point the next catch-up HTTP
                // call gives us a definitive signal.
                self.mark_offline();
            }
            Err(_) => {
                // Other ApiError variants (Status with a 4xx/5xx,
                // Decode, etc.) imply the server DID respond, so
                // the network is up. Leave state unchanged.
            }
        }
        self.get()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_is_unknown_and_treated_as_online_for_first_attempt() {
        let s = OnlineState::new();
        assert_eq!(s.get(), ConnectivityState::Unknown);
        assert!(
            s.is_online(),
            "Unknown must return true so the first request runs"
        );
    }

    #[tokio::test]
    async fn transport_error_marks_offline_success_restores() {
        // Drive a REAL reqwest::Error by attempting to connect to a
        // closed port on localhost -- we don't want to mock the
        // error shape because the bug we're guarding against is "the
        // match arm for Transport stops matching after a reqwest
        // upgrade". A real error proves the integration.
        let bad: Result<(), zk_sync_api::ApiError> = match reqwest::Client::new()
            .get("http://127.0.0.1:1/")
            .timeout(std::time::Duration::from_millis(100))
            .send()
            .await
        {
            Ok(_) => panic!("dial to closed port 1 must fail"),
            Err(e) => Err(zk_sync_api::ApiError::Transport(e)),
        };
        let s = OnlineState::new();
        s.record_api_result(&bad);
        assert_eq!(s.get(), ConnectivityState::Offline);
        assert!(!s.is_online());

        let good: Result<(), zk_sync_api::ApiError> = Ok(());
        s.record_api_result(&good);
        assert_eq!(s.get(), ConnectivityState::Online);
        assert!(s.is_online());
    }

    #[test]
    fn http_status_error_leaves_state_unchanged() {
        let s = OnlineState::new();
        s.mark_online();
        let four_oh_four: Result<(), zk_sync_api::ApiError> = Err(zk_sync_api::ApiError::Status {
            status: 404,
            body: "not found".into(),
        });
        s.record_api_result(&four_oh_four);
        // A 404 means the server responded -- we ARE online.
        assert_eq!(s.get(), ConnectivityState::Online);
    }

    #[test]
    fn websocket_error_marks_offline() {
        let s = OnlineState::new();
        s.mark_online();
        let ws: Result<(), zk_sync_api::ApiError> = Err(zk_sync_api::ApiError::WebSocket(
            "dial: tcp connect: refused".into(),
        ));
        s.record_api_result(&ws);
        assert_eq!(s.get(), ConnectivityState::Offline);
    }

    #[test]
    fn clone_shares_state() {
        let a = OnlineState::new();
        let b = a.clone();
        a.mark_offline();
        assert_eq!(b.get(), ConnectivityState::Offline);
        b.mark_online();
        assert_eq!(a.get(), ConnectivityState::Online);
    }
}
