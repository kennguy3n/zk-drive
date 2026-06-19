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
use serde::de::{self, Deserializer};
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

/// Live event decoded from the multiplexed `/api/ws` socket. The
/// backend tags every frame with a `type` and an arbitrary `payload`.
/// The same socket carries change-feed mutations (`type = "change"`)
/// alongside per-user notifications (`type = "notification"`) and
/// other event families (see `api/ws/handler.go` and
/// `internal/changefeed/publisher.go`), so the change-feed consumer
/// decodes `change` frames and ignores the rest.
#[derive(Debug, Clone)]
pub enum ChangeEvent {
    Change(Mutation),
    /// Server-sent ping; clients should ignore it (the underlying
    /// `tokio_tungstenite` already replies pong at the protocol
    /// level — this is a higher-level liveness signal).
    Heartbeat {},
    /// Any other frame multiplexed onto the shared socket
    /// (`notification`, upload progress, awareness, ...). Captured as
    /// a catch-all so an event family this client does not model is
    /// dropped by the consumer, rather than surfacing as a decode
    /// error that would tear down the live subscription on the very
    /// first notification frame.
    Other,
}

impl<'de> Deserialize<'de> for ChangeEvent {
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        // Every `/api/ws` frame shares this envelope: a `type`
        // discriminator plus an arbitrary `payload`. We route on the
        // discriminator and only decode the payload for frames we
        // model. Decoding through this intermediate (rather than a
        // `#[serde(tag, content)]` derive) is what makes unknown
        // frames tolerable: an adjacently-tagged enum eagerly
        // deserializes `payload` into the matched variant, so a
        // `#[serde(other)]` unit variant rejects any unknown frame
        // that carries a payload object — which notifications always
        // do — and that error would tear the live stream down.
        #[derive(Deserialize)]
        struct Envelope {
            #[serde(rename = "type")]
            kind: String,
            #[serde(default)]
            payload: Option<serde_json::Value>,
        }

        let envelope = Envelope::deserialize(deserializer)?;
        match envelope.kind.as_str() {
            "change" => {
                let payload = envelope
                    .payload
                    .ok_or_else(|| de::Error::missing_field("payload"))?;
                let mutation = serde_json::from_value(payload).map_err(de::Error::custom)?;
                Ok(ChangeEvent::Change(mutation))
            }
            "heartbeat" => Ok(ChangeEvent::Heartbeat {}),
            _ => Ok(ChangeEvent::Other),
        }
    }
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

    /// `GET /api/changes?since={since}&limit={limit}`
    ///
    /// Returns one page of mutations. `since` is the last sequence
    /// the caller has durably persisted (use `0` on first connect).
    /// `limit` is clamped server-side to `[1, MaxLimit]`; pass `None`
    /// to let the server pick its default page size.
    ///
    /// The caller's workspace is resolved server-side from the bearer
    /// token (the auth middleware injects it from the JWT claims), so
    /// the path carries no workspace id.
    pub async fn list_changes(&self, since: i64, limit: Option<u32>) -> Result<ChangefeedPage> {
        let mut url = join(self.client.base(), "api/changes")?;
        {
            let mut q = url.query_pairs_mut();
            q.append_pair("since", &since.to_string());
            if let Some(l) = limit {
                q.append_pair("limit", &l.to_string());
            }
        }
        let resp = self
            .client
            .request(reqwest::Method::GET, url)
            .await?
            .send()
            .await?;
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

    /// `WS /api/ws[?since={cursor}]`
    ///
    /// Returns an async stream of [`ChangeEvent`]. The stream
    /// terminates with `Err(ApiError::WebSocket(_))` on protocol
    /// failure or with `None` on a clean server-initiated close.
    /// Callers reconnect with the highest observed `sequence` as
    /// `since` to resume cleanly.
    ///
    /// `since` closes the gap between catch-up completion and the
    /// WebSocket handshake. Any mutation with `sequence > since` that
    /// the server has already persisted MUST be replayed on the wire
    /// before the connection transitions to "live" mode. The server
    /// is responsible for ordering: a client that observes a stream
    /// of mutations all with `sequence > since`, monotonically
    /// increasing, can persist the cursor on each frame and treat a
    /// dropped connection as recoverable via another catch-up + this
    /// stream call with the new highest-observed `sequence`.
    ///
    /// `since = None` (or `Some(0)`) requests "deliver from the
    /// beginning of retained history". This is the right value for a
    /// fresh sync agent that has nothing in its local catalogue;
    /// established agents always pass `Some(catalogue.cursor)`.
    ///
    /// The bearer token is fetched from the client's
    /// [`crate::transport::TokenProvider`] **at handshake time**, so
    /// reconnect-on-disconnect picks up freshly-refreshed tokens
    /// without callers having to thread a new string through.
    pub async fn stream_changes(&self, since: Option<i64>) -> Result<ChangeEventStream> {
        let mut url = stream_changes_url(self.client.base(), since)?;
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

        let mut req_builder = tungstenite::handshake::client::Request::builder()
            .uri(url.as_str())
            .header("Host", url.host_str().unwrap_or(""))
            .header("Upgrade", "websocket")
            .header("Connection", "Upgrade")
            .header(
                "Sec-WebSocket-Key",
                tungstenite::handshake::client::generate_key(),
            )
            .header("Sec-WebSocket-Version", "13");
        if let Some(token) = self.client.access_token().await? {
            req_builder = req_builder.header("Authorization", format!("Bearer {token}"));
        }
        let req = req_builder
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

/// Builds the WebSocket subscription URL with optional `since` cursor.
/// Extracted from [`ChangefeedClient::stream_changes`] so the wire
/// shape can be tested without standing up a real WebSocket server.
///
/// `since = None` omits the query parameter entirely, leaving the
/// URL unchanged from the baseline contract. A server that
/// hasn't yet implemented gap-replay sees the same URL it saw
/// before (forward-compat) and a server that has will fall back to
/// its own default ("from the beginning"). `since = Some(seq)`
/// appends `?since={normalized}` where negative values clip to 0.
///
/// The returned URL still has its original scheme (`http`/`https`);
/// the caller flips it to `ws`/`wss` after building.
fn stream_changes_url(base: &url::Url, since: Option<i64>) -> Result<url::Url> {
    let mut url = join(base, "api/ws")?;
    if let Some(seq) = since {
        let normalized = seq.max(0);
        url.query_pairs_mut()
            .append_pair("since", &normalized.to_string());
    }
    Ok(url)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stream_url_includes_since_when_provided() {
        // Closes the gap between catch-up completion and
        // WebSocket handshake. The server contract is "replay
        // anything with sequence > since before going live"; this
        // test pins the wire shape we send so a future refactor of
        // the URL builder doesn't drop the query param silently
        // (which would reintroduce the gap and look indistinguishable
        // from "WS endpoint is just dropping events").
        let base = url::Url::parse("https://api.example.test/").unwrap();
        let url = stream_changes_url(&base, Some(4242)).unwrap();
        assert_eq!(url.as_str(), "https://api.example.test/api/ws?since=4242",);
    }

    #[test]
    fn stream_url_omits_since_when_none() {
        // None is the "fresh agent, no cursor yet" signal. We
        // omit the query parameter entirely rather than sending
        // `?since=0` so an older server that hasn't picked up the
        // gap-replay contract sees exactly the URL it saw before
        // and can't accidentally 400 on an unrecognized query key.
        let base = url::Url::parse("https://api.example.test/").unwrap();
        let url = stream_changes_url(&base, None).unwrap();
        assert_eq!(url.query(), None);
        assert_eq!(url.path(), "/api/ws");
    }

    #[test]
    fn stream_url_clips_negative_since_to_zero() {
        // Defensive: a corrupted catalogue could conceivably surface
        // a negative cursor (i64 ColumnType in SQLite is signed and
        // we don't apply a CHECK constraint at write time). Rather
        // than send `?since=-7` for a strict server to 400 on, we
        // clip to 0 -- the contract is "deliver everything with
        // sequence > since", and any negative value satisfies that
        // for every real mutation.
        let base = url::Url::parse("https://api.example.test/").unwrap();
        let url = stream_changes_url(&base, Some(-7)).unwrap();
        assert!(url.as_str().ends_with("?since=0"));
    }

    #[test]
    fn mutation_round_trips_through_canonical_json() {
        // Sample matches the wire format produced by the server's
        // changefeed.Mutation. Adding a key here that the
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

    #[test]
    fn change_event_tolerates_multiplexed_non_change_frames() {
        // `/api/ws` is a multiplexed socket: the change feed shares it
        // with per-user notifications and other event families (see
        // `api/ws/handler.go`). A frame this client does not model must
        // decode to `Other` and be dropped — never surface as a decode
        // error, which would tear down the live subscription and force
        // a reconnect on every notification.
        let notification = r#"{
            "type": "notification",
            "payload": {
                "id": "abc",
                "type": "share_link.created",
                "title": "Shared",
                "body": "A file was shared with you"
            }
        }"#;
        assert!(matches!(
            serde_json::from_str::<ChangeEvent>(notification).unwrap(),
            ChangeEvent::Other
        ));

        // Heartbeats stay their own variant (consumers may treat them
        // as an explicit liveness signal), and an unknown type with no
        // payload at all still decodes cleanly to `Other`.
        assert!(matches!(
            serde_json::from_str::<ChangeEvent>(r#"{"type":"heartbeat"}"#).unwrap(),
            ChangeEvent::Heartbeat {}
        ));
        assert!(matches!(
            serde_json::from_str::<ChangeEvent>(r#"{"type":"file_upload"}"#).unwrap(),
            ChangeEvent::Other
        ));
    }
}
