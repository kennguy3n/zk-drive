//! Remote change-feed consumer.
//!
//! The poller runs two strategies in series:
//!
//!   1. On startup: cursor-paged catch-up via
//!      [`zk_sync_api::ChangefeedClient::list_changes`] until the
//!      server reports `has_more=false`.
//!   2. After catch-up: live WebSocket subscription via
//!      [`zk_sync_api::ChangefeedClient::stream_changes`]. If the
//!      socket drops, the poller falls back to catch-up and retries
//!      with exponential backoff.
//!
//! Each consumed mutation is mapped to a [`RemoteEvent`] and pushed
//! to the engine's channel. The poller is responsible for advancing
//! the workspace cursor in the [`Catalogue`].

use std::sync::Arc;
use std::time::{Duration, Instant};

use futures::StreamExt;
use tokio::sync::mpsc;
use tokio::sync::Mutex;
use tracing::warn;
use uuid::Uuid;

use zk_sync_api::{ChangefeedClient, Client};

use crate::catalogue::Catalogue;
use crate::events::RemoteEvent;
use crate::Result;

/// Initial backoff for both catch-up and live retry loops.
const INITIAL_BACKOFF: Duration = Duration::from_millis(500);

/// Maximum backoff for both catch-up and live retry loops.
const MAX_BACKOFF: Duration = Duration::from_secs(30);

/// Minimum duration the live WebSocket loop must stay connected for
/// us to treat the session as "successful" and reset its backoff. If
/// the socket churns faster than this, the live backoff continues to
/// grow regardless of how often the HTTP catch-up succeeds — that
/// way a persistently broken WebSocket endpoint can't be papered over
/// by a working HTTP path.
const LIVE_SUCCESS_THRESHOLD: Duration = Duration::from_secs(15);

pub struct RemotePoller {
    pub workspace_id: Uuid,
    pub client: Arc<Client>,
    pub catalogue: Arc<Mutex<Catalogue>>,
    pub page_size: u32,
}

impl RemotePoller {
    /// Runs the catch-up + live loop, pushing events to `tx`. Returns
    /// only when `tx` is closed (i.e. the engine shut down).
    ///
    /// Catch-up and live use **independent backoffs**. Resetting a
    /// shared backoff after every successful catch-up would defeat
    /// the live-loop's exponential backoff in the failure mode where
    /// the WebSocket endpoint is persistently down but the HTTP
    /// endpoint works — the live loop would never grow past the
    /// second step.
    pub async fn run(self, tx: mpsc::Sender<RemoteEvent>) -> Result<()> {
        let mut catch_up_backoff = INITIAL_BACKOFF;
        let mut live_backoff = INITIAL_BACKOFF;
        loop {
            if let Err(e) = self.catch_up(&tx).await {
                warn!("changefeed catch-up failed: {e:?}");
                tokio::time::sleep(catch_up_backoff).await;
                catch_up_backoff = (catch_up_backoff * 2).min(MAX_BACKOFF);
                continue;
            }
            catch_up_backoff = INITIAL_BACKOFF;

            let started = Instant::now();
            let live_result = self.live(&tx).await;
            let ran_for = started.elapsed();
            match live_result {
                Ok(_) if ran_for >= LIVE_SUCCESS_THRESHOLD => {
                    live_backoff = INITIAL_BACKOFF;
                }
                Ok(_) => {
                    // Disconnect happened too quickly to count as a
                    // healthy session — grow live backoff anyway.
                    warn!(
                        "changefeed live subscription closed after {:?}; growing backoff",
                        ran_for
                    );
                    tokio::time::sleep(live_backoff).await;
                    live_backoff = (live_backoff * 2).min(MAX_BACKOFF);
                }
                Err(e) => {
                    warn!("changefeed live subscription dropped: {e:?}");
                    tokio::time::sleep(live_backoff).await;
                    live_backoff = (live_backoff * 2).min(MAX_BACKOFF);
                }
            }
        }
    }

    async fn catch_up(&self, tx: &mpsc::Sender<RemoteEvent>) -> Result<()> {
        loop {
            let since = self.catalogue.lock().await.get_cursor(self.workspace_id)?;
            let page = ChangefeedClient::new(&self.client)
                .list_changes(self.workspace_id, since, Some(self.page_size))
                .await?;
            if page.mutations.is_empty() && !page.has_more {
                self.catalogue
                    .lock()
                    .await
                    .set_cursor(self.workspace_id, page.cursor)?;
                return Ok(());
            }
            for m in page.mutations {
                let ev = RemoteEvent::from_mutation(m);
                if tx.send(ev).await.is_err() {
                    return Ok(()); // consumer gone
                }
            }
            self.catalogue
                .lock()
                .await
                .set_cursor(self.workspace_id, page.cursor)?;
            if !page.has_more {
                return Ok(());
            }
        }
    }

    async fn live(&self, tx: &mpsc::Sender<RemoteEvent>) -> Result<()> {
        let mut stream = ChangefeedClient::new(&self.client)
            .stream_changes(self.workspace_id)
            .await?;
        while let Some(item) = stream.next().await {
            match item? {
                zk_sync_api::ChangeEvent::Change(m) => {
                    let seq = m.sequence;
                    let ev = RemoteEvent::from_mutation(m);
                    if tx.send(ev).await.is_err() {
                        return Ok(());
                    }
                    self.catalogue
                        .lock()
                        .await
                        .set_cursor(self.workspace_id, seq)?;
                }
                zk_sync_api::ChangeEvent::Heartbeat {} => {}
            }
        }
        Ok(())
    }
}
