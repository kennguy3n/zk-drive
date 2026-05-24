//! Presigned upload / download URL negotiation.
//!
//! For Strict-ZK files the sync engine encrypts locally (via the
//! `crypto` crate) and uploads the ciphertext via the presigned PUT
//! URL returned by `request_upload`. For ManagedEncrypted files the
//! engine uploads plaintext and the server encrypts behind its KMS
//! envelope.

use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::error::{ApiError, Result};
use crate::transport::{join, Client};

/// One presigned upload URL with the headers the storage backend
/// requires the client to echo back on PUT.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PresignedUpload {
    pub upload_id: Uuid,
    pub url: String,
    /// HTTP method to use (almost always `PUT`).
    pub method: String,
    /// Headers the client MUST set verbatim — Content-Type,
    /// x-amz-checksum-sha256, etc. Names are case-insensitive.
    #[serde(default)]
    pub headers: std::collections::BTreeMap<String, String>,
    /// Unix epoch seconds at which `url` stops being valid. Clients
    /// should refresh by re-calling `request_upload` if they're
    /// close.
    pub expires_at: i64,
}

/// One presigned download URL.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PresignedDownload {
    pub url: String,
    pub expires_at: i64,
    /// Total content size in bytes (the manifest's pre-encryption
    /// length for managed; the ciphertext length for strict-zk).
    pub size_bytes: u64,
}

pub struct StorageClient<'c> {
    client: &'c Client,
}

impl<'c> StorageClient<'c> {
    pub fn new(client: &'c Client) -> Self {
        Self { client }
    }

    /// `POST /api/v1/files/{file_id}/uploads`
    ///
    /// Negotiates a presigned PUT URL for either the next version of
    /// an existing file (file_id present) or a brand-new file (the
    /// caller has previously created an empty placeholder via
    /// `FsClient::create_file`).
    pub async fn request_upload(&self, file_id: Uuid, size_bytes: u64) -> Result<PresignedUpload> {
        #[derive(Serialize)]
        struct Req {
            size_bytes: u64,
        }
        let path = format!("api/v1/files/{file_id}/uploads");
        let url = join(self.client.base(), &path)?;
        let resp = self
            .client
            .http
            .post(url)
            .json(&Req { size_bytes })
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
        serde_json::from_str(&body).map_err(|e| ApiError::Decode(format!("upload: {e}")))
    }

    /// `GET /api/v1/files/{file_id}/downloads`
    pub async fn request_download(&self, file_id: Uuid) -> Result<PresignedDownload> {
        let path = format!("api/v1/files/{file_id}/downloads");
        let url = join(self.client.base(), &path)?;
        let resp = self.client.http.get(url).send().await?;
        let status = resp.status();
        let body = resp.text().await?;
        if !status.is_success() {
            return Err(ApiError::Status {
                status: status.as_u16(),
                body,
            });
        }
        serde_json::from_str(&body).map_err(|e| ApiError::Decode(format!("download: {e}")))
    }
}
