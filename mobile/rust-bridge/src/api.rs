//! HTTP surface against the live zk-drive backend.
//!
//! This wraps [`zk_sync_api::Client`] purely for its authenticated
//! transport (bearer injection via the shared [`TokenManager`], TLS,
//! timeouts) and then speaks the backend's real REST contract:
//! `/api/files/upload-url`, `/api/files/{id}/download-url`,
//! `/api/files/{id}/preview-url`, `/api/files/confirm-upload` and the
//! `/api/changes` catch-up feed. (The SDK's higher-level
//! `StorageClient` / `FsClient` target a different `/api/v1/...`
//! surface that this server does not expose, so the bridge talks to
//! the deployed endpoints directly.)
//!
//! Every networked method blocks the calling thread on the shared
//! runtime; callers MUST invoke them off the platform UI thread.

use std::sync::Arc;

use reqwest::Method;
use serde::{Deserialize, Serialize};
use zk_sync_api::{Client, Mutation};

use crate::auth::TokenManager;
use crate::error::{BridgeError, Result};
use crate::runtime::block_on;

/// Presigned direct-to-storage upload target. The native client PUTs
/// the (already client-side-encrypted) bytes to `upload_url`, then
/// calls [`ApiClient::confirm_upload`] echoing `file_id` + `object_key`.
#[derive(Debug, Clone, uniffi::Record)]
pub struct UploadTarget {
    pub upload_url: String,
    /// Stable file id (the backend's `upload_id`) to reference on
    /// confirm and in subsequent download / preview calls.
    pub file_id: String,
    /// Opaque storage key; must be echoed back verbatim on confirm.
    pub object_key: String,
}

/// Presigned direct-from-storage download target for the current
/// version of a file.
#[derive(Debug, Clone, uniffi::Record)]
pub struct DownloadTarget {
    pub download_url: String,
    pub object_key: String,
}

/// Presigned download target for a file's generated preview rendition.
#[derive(Debug, Clone, uniffi::Record)]
pub struct PreviewTarget {
    pub preview_url: String,
    pub object_key: String,
    pub mime_type: String,
}

/// One catch-up page of changefeed mutations. `cursor` is the sequence
/// to pass as `since` on the next poll; `has_more` indicates the server
/// truncated the page at the limit and another poll should follow
/// immediately rather than waiting for the next interval.
#[derive(Debug, Clone, uniffi::Record)]
pub struct ChangePage {
    pub mutations: Vec<ChangeRecord>,
    pub cursor: i64,
    pub has_more: bool,
}

/// FFI-flattened changefeed mutation. UUID and timestamp fields are
/// rendered to FFI-friendly primitives (hyphenated UUID strings,
/// Unix-millis timestamps) and `metadata` is passed through as its raw
/// JSON string so the native layer can lazily parse only what it needs.
#[derive(Debug, Clone, uniffi::Record)]
pub struct ChangeRecord {
    pub sequence: i64,
    pub workspace_id: String,
    pub actor_id: Option<String>,
    pub kind: String,
    pub op: String,
    pub resource_id: String,
    pub parent_id: Option<String>,
    pub name: String,
    pub metadata_json: Option<String>,
    pub occurred_at_unix_ms: i64,
}

impl From<Mutation> for ChangeRecord {
    fn from(m: Mutation) -> Self {
        Self {
            sequence: m.sequence,
            workspace_id: m.workspace_id.to_string(),
            actor_id: m.actor_id.map(|id| id.to_string()),
            kind: m.kind,
            op: m.op,
            resource_id: m.resource_id.to_string(),
            parent_id: m.parent_id.map(|id| id.to_string()),
            name: m.name,
            metadata_json: m.metadata.map(|v| v.to_string()),
            occurred_at_unix_ms: m.occurred_at.timestamp_millis(),
        }
    }
}

/// Authenticated HTTP client bound to one backend base URL and one
/// [`TokenManager`]. Cheap to clone-share via `Arc`; the sync engine
/// holds one to drive changefeed polling and transfer URL minting.
#[derive(uniffi::Object)]
pub struct ApiClient {
    client: Client,
}

#[uniffi::export]
impl ApiClient {
    /// Build a client for `base_url` (e.g. `https://api.zkdrive.example.com`)
    /// that authenticates every request with a fresh bearer from
    /// `tokens`.
    #[uniffi::constructor]
    pub fn new(base_url: String, tokens: Arc<TokenManager>) -> Result<Arc<Self>> {
        let client = Client::builder(&base_url)
            .token_provider(tokens.token_provider())
            .user_agent(concat!("zk-mobile-bridge/", env!("CARGO_PKG_VERSION")))
            .build()
            .map_err(BridgeError::from)?;
        Ok(Arc::new(Self { client }))
    }

    /// Create file metadata and mint a presigned PUT URL for a new
    /// upload into `folder_id`. `POST /api/files/upload-url`.
    pub fn upload_url(
        &self,
        folder_id: String,
        filename: String,
        mime_type: Option<String>,
    ) -> Result<UploadTarget> {
        #[derive(Serialize)]
        struct Req {
            folder_id: String,
            filename: String,
            #[serde(skip_serializing_if = "Option::is_none")]
            mime_type: Option<String>,
        }
        #[derive(Deserialize)]
        struct Resp {
            upload_url: String,
            upload_id: String,
            object_key: String,
        }
        let resp: Resp = block_on(self.send(
            Method::POST,
            "api/files/upload-url",
            &[],
            Some(&Req {
                folder_id,
                filename,
                mime_type,
            }),
        ))?;
        Ok(UploadTarget {
            upload_url: resp.upload_url,
            file_id: resp.upload_id,
            object_key: resp.object_key,
        })
    }

    /// Record a completed direct-to-storage upload as the file's new
    /// current version. `POST /api/files/confirm-upload`. Returns the
    /// new version id. MUST be called after the PUT to `upload_url`
    /// succeeds or the file row stays version-less and downloads 404.
    pub fn confirm_upload(
        &self,
        file_id: String,
        object_key: String,
        size_bytes: i64,
        checksum: Option<String>,
    ) -> Result<String> {
        #[derive(Serialize)]
        struct Req {
            file_id: String,
            object_key: String,
            size_bytes: i64,
            #[serde(skip_serializing_if = "Option::is_none")]
            checksum: Option<String>,
        }
        #[derive(Deserialize)]
        struct Version {
            id: String,
        }
        #[derive(Deserialize)]
        struct Resp {
            version: Version,
        }
        let resp: Resp = block_on(self.send(
            Method::POST,
            "api/files/confirm-upload",
            &[],
            Some(&Req {
                file_id,
                object_key,
                size_bytes,
                checksum,
            }),
        ))?;
        Ok(resp.version.id)
    }

    /// Mint a presigned GET URL for the current version of `file_id`.
    /// `GET /api/files/{id}/download-url`.
    pub fn download_url(&self, file_id: String) -> Result<DownloadTarget> {
        #[derive(Deserialize)]
        struct Resp {
            download_url: String,
            object_key: String,
        }
        let path = format!("api/files/{}/download-url", encode_segment(&file_id)?);
        let resp: Resp = block_on(self.send::<(), _>(Method::GET, &path, &[], None))?;
        Ok(DownloadTarget {
            download_url: resp.download_url,
            object_key: resp.object_key,
        })
    }

    /// Mint a presigned GET URL for the latest generated preview of
    /// `file_id`. `GET /api/files/{id}/preview-url`. Errors with
    /// [`BridgeError::Api`] status 404 when no preview exists yet.
    pub fn preview_url(&self, file_id: String) -> Result<PreviewTarget> {
        #[derive(Deserialize)]
        struct Resp {
            preview_url: String,
            object_key: String,
            mime_type: String,
        }
        let path = format!("api/files/{}/preview-url", encode_segment(&file_id)?);
        let resp: Resp = block_on(self.send::<(), _>(Method::GET, &path, &[], None))?;
        Ok(PreviewTarget {
            preview_url: resp.preview_url,
            object_key: resp.object_key,
            mime_type: resp.mime_type,
        })
    }

    /// Fetch one catch-up page of changefeed mutations with
    /// `sequence > since`. `GET /api/changes?since={cursor}&limit={n}`.
    /// The workspace is resolved server-side from the bearer.
    pub fn get_changes(&self, since: i64, limit: Option<u32>) -> Result<ChangePage> {
        block_on(self.changes_async(since, limit))
    }
}

impl ApiClient {
    /// Async core of [`ApiClient::get_changes`]. The sync engine's
    /// continuous-poll loop awaits this directly: it already runs on the
    /// shared runtime, so calling the blocking wrapper there would panic
    /// ("cannot block the current thread from within a runtime").
    pub(crate) async fn changes_async(
        &self,
        since: i64,
        limit: Option<u32>,
    ) -> Result<ChangePage> {
        #[derive(Deserialize)]
        struct Resp {
            #[serde(default)]
            mutations: Vec<Mutation>,
            cursor: i64,
            has_more: bool,
        }
        let mut query: Vec<(&str, String)> = vec![("since", since.to_string())];
        if let Some(n) = limit {
            query.push(("limit", n.to_string()));
        }
        let resp: Resp = self
            .send::<(), _>(Method::GET, "api/changes", &query, None)
            .await?;
        Ok(ChangePage {
            mutations: resp.mutations.into_iter().map(ChangeRecord::from).collect(),
            cursor: resp.cursor,
            has_more: resp.has_more,
        })
    }

    /// Shared request helper: resolve the URL against the base, attach a
    /// fresh bearer, send an optional JSON body, then map the HTTP
    /// status into the bridge's typed errors and decode the JSON
    /// response. `B = ()` for bodyless GETs.
    async fn send<B, T>(
        &self,
        method: Method,
        path: &str,
        query: &[(&str, String)],
        body: Option<&B>,
    ) -> Result<T>
    where
        B: Serialize + ?Sized,
        T: serde::de::DeserializeOwned,
    {
        let mut url = self.client.base().join(path)?;
        if !query.is_empty() {
            let mut pairs = url.query_pairs_mut();
            for (k, v) in query {
                pairs.append_pair(k, v);
            }
        }
        let mut builder = self
            .client
            .request(method, url)
            .await
            .map_err(BridgeError::from)?;
        if let Some(b) = body {
            builder = builder.json(b);
        }
        let resp = builder.send().await.map_err(|e| BridgeError::Network(e.to_string()))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| BridgeError::Network(e.to_string()))?;
        if !status.is_success() {
            return Err(BridgeError::Api {
                status: status.as_u16(),
                message: text,
            });
        }
        serde_json::from_str(&text)
            .map_err(|e| BridgeError::Api {
                status: status.as_u16(),
                message: format!("decode response: {e}"),
            })
    }
}

/// Reject path segments that would let a caller escape the intended
/// route (slashes, dot-dot, control chars). File ids are UUIDs, so any
/// of these means a malformed argument.
fn encode_segment(seg: &str) -> Result<&str> {
    if seg.is_empty()
        || seg.contains('/')
        || seg.contains('\\')
        || seg.contains("..")
        || seg.chars().any(|c| c.is_control())
    {
        return Err(BridgeError::InvalidInput(format!(
            "invalid path segment: {seg:?}"
        )));
    }
    Ok(seg)
}
