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
use std::time::Duration;

use futures::StreamExt;
use tokio::sync::mpsc;
use tokio::sync::Mutex;
use tracing::warn;
use uuid::Uuid;

use zk_sync_api::{ChangefeedClient, Client};

use crate::catalogue::Catalogue;
use crate::events::RemoteEvent;
use crate::Result;

pub struct RemotePoller {
    pub workspace_id: Uuid,
    pub client: Arc<Client>,
    pub catalogue: Arc<Mutex<Catalogue>>,
    pub bearer: String,
    pub page_size: u32,
}

impl RemotePoller {
    /// Runs the catch-up + live loop, pushing events to `tx`. Returns
    /// only when `tx` is closed (i.e. the engine shut down).
    pub async fn run(self, tx: mpsc::Sender<RemoteEvent>) -> Result<()> {
        let mut backoff = Duration::from_millis(500);
        loop {
            if let Err(e) = self.catch_up(&tx).await {
                warn!("changefeed catch-up failed: {e:?}");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
                continue;
            }
            backoff = Duration::from_millis(500);
            if let Err(e) = self.live(&tx).await {
                warn!("changefeed live subscription dropped: {e:?}");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
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
            .stream_changes(self.workspace_id, &self.bearer)
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
