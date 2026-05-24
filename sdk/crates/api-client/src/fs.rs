//! File / folder metadata CRUD client.

use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::error::{ApiError, Result};
use crate::transport::{join, Client};

/// File metadata. Mirrors the JSON shape returned by
/// `api/drive/file.go`'s handlers. Optional fields are encoded with
/// `omitempty` on the server side, so the Rust struct uses Options.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct File {
    pub id: Uuid,
    pub workspace_id: Uuid,
    pub folder_id: Uuid,
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mime_type: Option<String>,
    #[serde(default)]
    pub size_bytes: u64,
    /// Encryption mode of the parent folder. Sync clients route on
    /// this — Strict-ZK requires local encrypt/decrypt before
    /// presigned PUT/GET.
    pub encryption_mode: String,
    pub current_version_id: Uuid,
    pub created_at: chrono::DateTime<chrono::Utc>,
    pub updated_at: chrono::DateTime<chrono::Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FileVersion {
    pub id: Uuid,
    pub file_id: Uuid,
    pub size_bytes: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content_sha256: Option<String>,
    pub created_at: chrono::DateTime<chrono::Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Folder {
    pub id: Uuid,
    pub workspace_id: Uuid,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent_folder_id: Option<Uuid>,
    pub name: String,
    pub encryption_mode: String,
    pub created_at: chrono::DateTime<chrono::Utc>,
    pub updated_at: chrono::DateTime<chrono::Utc>,
}

pub struct FsClient<'c> {
    client: &'c Client,
}

impl<'c> FsClient<'c> {
    pub fn new(client: &'c Client) -> Self {
        Self { client }
    }

    /// `GET /api/v1/files/{file_id}`
    pub async fn get_file(&self, file_id: Uuid) -> Result<File> {
        let url = join(self.client.base(), &format!("api/v1/files/{file_id}"))?;
        let resp = self.client.http.get(url).send().await?;
        let status = resp.status();
        let body = resp.text().await?;
        if !status.is_success() {
            return Err(ApiError::Status {
                status: status.as_u16(),
                body,
            });
        }
        serde_json::from_str(&body).map_err(|e| ApiError::Decode(format!("file: {e}")))
    }

    /// `GET /api/v1/folders/{folder_id}`
    pub async fn get_folder(&self, folder_id: Uuid) -> Result<Folder> {
        let url = join(self.client.base(), &format!("api/v1/folders/{folder_id}"))?;
        let resp = self.client.http.get(url).send().await?;
        let status = resp.status();
        let body = resp.text().await?;
        if !status.is_success() {
            return Err(ApiError::Status {
                status: status.as_u16(),
                body,
            });
        }
        serde_json::from_str(&body).map_err(|e| ApiError::Decode(format!("folder: {e}")))
    }

    /// `POST /api/v1/files`
    ///
    /// Creates a brand-new file row. Returns the placeholder; the
    /// caller follows up with `StorageClient::request_upload` and
    /// uploads bytes via the presigned URL.
    pub async fn create_file(
        &self,
        folder_id: Uuid,
        name: &str,
        mime_type: Option<&str>,
    ) -> Result<File> {
        #[derive(Serialize)]
        struct Req<'a> {
            folder_id: Uuid,
            name: &'a str,
            #[serde(skip_serializing_if = "Option::is_none")]
            mime_type: Option<&'a str>,
        }
        let url = join(self.client.base(), "api/v1/files")?;
        let resp = self
            .client
            .http
            .post(url)
            .json(&Req {
                folder_id,
                name,
                mime_type,
            })
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
        serde_json::from_str(&body).map_err(|e| ApiError::Decode(format!("create file: {e}")))
    }
}
