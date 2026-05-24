//! Typed HTTP / WebSocket client for the ZK Drive backend.
//!
//! The client surfaces three responsibilities:
//!
//!   * [`ChangefeedClient`] consumes
//!     `GET /api/v1/workspaces/{id}/changes?since={cursor}` and
//!     `WS  /api/v1/workspaces/{id}/changes/stream` — the workspace
//!     change feed introduced in PR #73 (kennguy3n/zk-drive). The
//!     sync engine uses this to discover remote mutations.
//!   * [`StorageClient`] negotiates presigned URLs for direct-to-
//!     storage uploads / downloads (`POST /api/v1/files/{id}/uploads`,
//!     `GET  /api/v1/files/{id}/downloads`).
//!   * [`FsClient`] performs CRUD on file / folder metadata
//!     (`POST /api/v1/files`, `PATCH /api/v1/folders/{id}`, ...).
//!
//! All HTTP requests carry the `Authorization: Bearer …` header
//! managed by the `auth` crate's token store. The session token is
//! threaded as a [`Bearer`] value so the client is decoupled from the
//! exact token-acquisition mechanism (OAuth2 PKCE, service account,
//! test fixture).

pub mod changefeed;
mod error;
mod fs;
mod storage;
mod transport;

pub use changefeed::{ChangeEvent, ChangeEventStream, ChangefeedClient, ChangefeedPage, Mutation};
pub use error::{ApiError, Result};
pub use fs::{File, FileVersion, Folder, FsClient};
pub use storage::{PresignedDownload, PresignedUpload, StorageClient};
pub use transport::{Bearer, Client, ClientBuilder};
