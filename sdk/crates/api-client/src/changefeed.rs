//! Client for the workspace change feed.
//!
//! Wire format matches `internal/changefeed/changefeed.go` in
//! `kennguy3n/zk-drive`. The Rust types here MUST stay JSON-
//! compatible with the Go types — see the doc comments below for
//! invariants (`omitempty` on ParentID / Name / Metadata, sequence
//! always present, etc).

use std::pin::Pin;

use futures::stream::Stream;
use futures::StreamExt;
use serde::{Deserialize, Serialize};
use tokio_tungstenite::tungstenite;
use uuid::Uuid;

use crate::error::{ApiError, Result};
use crate::transport::{join, Client};

/// One persisted change_log row. Mirrors `changefeed.Mutation`.
///
/// `metadata` is left as opaque JSON (`serde_json::Value`) because
/// it's per-action free-form. Sync clients route on `kind` + `op`
/// and look at structured columns first; metadata is for the
/// occasional richer-context render.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Mutation {
    pub sequence: i64,
    pub workspace_id: Uuid,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub actor_id: Option<Uuid>,
    pub kind: String,
    pub op: String,
    pub resource_id: Uuid,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent_id: Option<Uuid>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub metadata: Option<serde_json::Value>,
    pub occurred_at: chrono::DateTime<chrono::Utc>,
}

/// `kind` strings recognised by the backend. Defined as constants
/// rather than an enum so unknown kinds (e.g. a new server-side
/// kind ahead of this SDK's release) don't fail JSON decoding.
pub mod kind {
    pub const FILE: &str = "file";
    pub const FOLDER: &str = "folder";
    pub const PERMISSION: &str = "permission";
}

/// `op` strings recognised by the backend.
pub mod op {
    pub const CREATE: &str = "create";
    pub const UPDATE: &str = "update";
    pub const RENAME: &str = "rename";
    pub const MOVE: &str = "move";
    pub const DELETE: &str = "delete";
}

/// Cursor-paged catch-up response. Mirrors `changefeed.Page`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ChangefeedPage {
    pub mutations: Vec<Mutation>,
    pub cursor: i64,
    pub has_more: bool,
}

/// Live event envelope pushed over WebSocket. The backend uses a
/// type-tagged JSON shape so the same socket can later multiplex
/// other event types (notifications, awareness, ...).
#[derive(Debug, Clone, Deserialize)]
#[serde(tag = "type", content = "payload", rename_all = "snake_case")]
pub enum ChangeEvent {
    Change(Mutation),
    /// Server-sent ping; clients should ignore it (the underlying
    /// `tokio_tungstenite` already replies pong at the protocol
    /// level — this is a higher-level liveness signal).
    Heartbeat {},
}

/// Owned async stream of [`ChangeEvent`]s. `Send + 'static` so the
/// sync engine can park it on a `tokio::spawn`.
pub type ChangeEventStream = Pin<Box<dyn Stream<Item = Result<ChangeEvent>> + Send + 'static>>;

/// Client for the workspace change feed.
pub struct ChangefeedClient<'c> {
    client: &'c Client,
}

impl<'c> ChangefeedClient<'c> {
    /// Construct from an existing [`Client`]. Cheap; just borrows.
    pub fn new(client: &'c Client) -> Self {
        Self { client }
    }

    /// `GET /api/v1/workspaces/{workspace_id}/changes?since={since}&limit={limit}`
    ///
    /// Returns one page of mutations. `since` is the last sequence
    /// the caller has durably persisted (use `0` on first connect).
    /// `limit` is clamped server-side to `[1, MaxLimit]`; pass `None`
    /// to let the server pick its default page size.
    pub async fn list_changes(
        &self,
        workspace_id: Uuid,
        since: i64,
        limit: Option<u32>,
    ) -> Result<ChangefeedPage> {
        let path = format!("api/v1/workspaces/{workspace_id}/changes");
        let mut url = join(self.client.base(), &path)?;
        {
            let mut q = url.query_pairs_mut();
            q.append_pair("since", &since.to_string());
            if let Some(l) = limit {
                q.append_pair("limit", &l.to_string());
            }
        }
        let resp = self.client.http.get(url).send().await?;
        let status = resp.status();
        let body = resp.text().await?;
        if !status.is_success() {
            return Err(ApiError::Status {
                status: status.as_u16(),
                body,
            });
        }
        serde_json::from_str(&body).map_err(|e| ApiError::Decode(format!("changefeed page: {e}")))
    }

    /// `WS /api/v1/workspaces/{workspace_id}/changes/stream`
    ///
    /// Returns an async stream of [`ChangeEvent`]. The stream
    /// terminates with `Err(ApiError::WebSocket(_))` on protocol
    /// failure or with `None` on a clean server-initiated close.
    /// Callers reconnect with the highest observed `sequence` as
    /// `since` to resume cleanly.
    pub async fn stream_changes(
        &self,
        workspace_id: Uuid,
        bearer_token: &str,
    ) -> Result<ChangeEventStream> {
        let mut url = join(
            self.client.base(),
            &format!("api/v1/workspaces/{workspace_id}/changes/stream",),
        )?;
        match url.scheme() {
            "http" => {
                url.set_scheme("ws")
                    .map_err(|_| ApiError::websocket("set scheme ws"))?;
            }
            "https" => {
                url.set_scheme("wss")
                    .map_err(|_| ApiError::websocket("set scheme wss"))?;
            }
            other => {
                return Err(ApiError::websocket(format!(
                    "unsupported base url scheme: {other}"
                )));
            }
        }

        let req = tungstenite::handshake::client::Request::builder()
            .uri(url.as_str())
            .header("Authorization", format!("Bearer {bearer_token}"))
            .header("Host", url.host_str().unwrap_or(""))
            .header("Upgrade", "websocket")
            .header("Connection", "Upgrade")
            .header(
                "Sec-WebSocket-Key",
                tungstenite::handshake::client::generate_key(),
            )
            .header("Sec-WebSocket-Version", "13")
            .body(())
            .map_err(|e| ApiError::websocket(format!("build ws request: {e}")))?;

        let (ws, _) = tokio_tungstenite::connect_async(req)
            .await
            .map_err(|e| ApiError::websocket(format!("dial: {e}")))?;
        let stream = ws.filter_map(|frame| async move {
            match frame {
                Ok(tungstenite::Message::Text(t)) => Some(
                    serde_json::from_str::<ChangeEvent>(&t)
                        .map_err(|e| ApiError::Decode(format!("ws frame: {e}"))),
                ),
                Ok(tungstenite::Message::Binary(b)) => Some(
                    serde_json::from_slice::<ChangeEvent>(&b)
                        .map_err(|e| ApiError::Decode(format!("ws frame (binary): {e}"))),
                ),
                Ok(tungstenite::Message::Close(_)) => None,
                Ok(_) => None, // ping/pong/raw — already handled by transport
                Err(e) => Some(Err(ApiError::websocket(format!("frame: {e}")))),
            }
        });
        Ok(Box::pin(stream))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mutation_round_trips_through_canonical_json() {
        // Sample matches the wire format produced by
        // changefeed.Mutation in PR #73. Adding a key here that the
        // Rust side doesn't know about must NOT fail decoding — the
        // metadata blob carries forward-compat data.
        let raw = r#"{
            "sequence": 42,
            "workspace_id": "11111111-1111-1111-1111-111111111111",
            "actor_id": "22222222-2222-2222-2222-222222222222",
            "kind": "file",
            "op": "create",
            "resource_id": "33333333-3333-3333-3333-333333333333",
            "parent_id": "44444444-4444-4444-4444-444444444444",
            "name": "report.docx",
            "metadata": {"size_bytes": 1024, "future_key": "tolerated"},
            "occurred_at": "2025-01-01T12:00:00Z"
        }"#;
        let m: Mutation = serde_json::from_str(raw).expect("parse");
        assert_eq!(m.sequence, 42);
        assert_eq!(m.kind, kind::FILE);
        assert_eq!(m.op, op::CREATE);
        assert_eq!(m.name, "report.docx");
        // omitempty on parent_id / name when absent.
        let minimal = r#"{
            "sequence": 7,
            "workspace_id": "11111111-1111-1111-1111-111111111111",
            "kind": "file",
            "op": "delete",
            "resource_id": "33333333-3333-3333-3333-333333333333",
            "occurred_at": "2025-01-01T12:00:00Z"
        }"#;
        let d: Mutation = serde_json::from_str(minimal).expect("parse minimal");
        assert!(d.parent_id.is_none());
        assert!(d.name.is_empty());
        assert!(d.metadata.is_none());
    }

    #[test]
    fn change_event_decodes_type_tagged_envelope() {
        let raw = r#"{
            "type": "change",
            "payload": {
                "sequence": 1,
                "workspace_id": "11111111-1111-1111-1111-111111111111",
                "kind": "folder",
                "op": "rename",
                "resource_id": "33333333-3333-3333-3333-333333333333",
                "occurred_at": "2025-01-01T12:00:00Z"
            }
        }"#;
        match serde_json::from_str::<ChangeEvent>(raw).unwrap() {
            ChangeEvent::Change(m) => assert_eq!(m.sequence, 1),
            other => panic!("unexpected variant: {other:?}"),
        }
    }
}
