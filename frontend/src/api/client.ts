import axios, { type AxiosInstance } from "axios";

// Shared Axios instance pointed at the dev proxy (/api -> :8080). All
// request/response types below match the Go handler JSON.
const client: AxiosInstance = axios.create({
  baseURL: "/api",
  headers: { "Content-Type": "application/json" },
});

const TOKEN_STORAGE_KEY = "zkdrive.token";
const WORKSPACE_STORAGE_KEY = "zkdrive.workspace_id";
const ROLE_STORAGE_KEY = "zkdrive.role";
const USER_STORAGE_KEY = "zkdrive.user_id";

// Attach the JWT to every outgoing request. Kept as a single interceptor
// so the token is always fresh (e.g. after login, the next request picks
// up the new value from localStorage without page reload).
client.interceptors.request.use((config) => {
  const token = localStorage.getItem(TOKEN_STORAGE_KEY);
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

// NON_SESSION_401_CODES enumerates the structured error codes that come
// back with HTTP 401 but DO NOT indicate the user's session is dead.
// They're "soft" auth failures — the caller needs to provide something
// other than a new login (a share-link password, a workspace header) —
// so we surface the error to the calling page instead of nuking auth
// state and bouncing to /login. Without this guard, e.g. an
// unauthenticated visitor opening a password-protected share link
// would get redirected to /login (clearing nothing, since they had no
// session) instead of seeing the password prompt.
const NON_SESSION_401_CODES = new Set<string>([
  // Share-link password challenge. The user is browsing a public share
  // and needs to enter the link's password. Redirecting to /login would
  // be actively wrong: they have no account here.
  "SHARE_PASSWORD_REQUIRED",
  // The request reached the API authenticated but without a workspace
  // header. The session is still valid; the page just needs to retry
  // with a workspace selected. Clearing the token here would force a
  // re-login for a recoverable routing error.
  "MISSING_WORKSPACE_CONTEXT",
]);

// Redirect to /login on session-expiry 401s so stale sessions don't
// leave the UI stuck. Clear ALL auth-derived localStorage keys (token,
// workspace, user_id) so the next login is a clean slate; otherwise a
// stale user_id could persist into a different user's session and break
// presence (cursor colors keyed on the wrong id, PresenceChips failing
// to filter the local user, etc.).
//
// We treat 401 as "session expired" UNLESS the structured error code
// indicates a soft auth failure that the calling page should handle
// (see NON_SESSION_401_CODES). The structured-code carve-out replaces
// the old "always redirect on 401" behaviour that misrouted
// password-protected share links and missing-workspace responses
// through the login flow.
client.interceptors.response.use(
  (resp) => resp,
  (err) => {
    if (err?.response?.status === 401) {
      const data = err.response.data as { code?: string } | undefined;
      const code = typeof data?.code === "string" ? data.code : null;
      if (!code || !NON_SESSION_401_CODES.has(code)) {
        localStorage.removeItem(TOKEN_STORAGE_KEY);
        localStorage.removeItem(WORKSPACE_STORAGE_KEY);
        localStorage.removeItem(ROLE_STORAGE_KEY);
        localStorage.removeItem(USER_STORAGE_KEY);
        if (window.location.pathname !== "/login") {
          window.location.href = "/login";
        }
      }
    }
    return Promise.reject(err);
  },
);

// --- Domain types --------------------------------------------------------

export interface AuthResponse {
  token: string;
  user_id: string;
  workspace_id: string;
  role: string;
}

// EncryptionMode is the canonical wire-level union for a folder's
// privacy mode. The server emits one of these two values; older
// folder rows may omit the field entirely. The two strings are part
// of the public API surface — widening this with `| string` would
// silently break TS narrowing in every consumer, so we keep it strict
// here and let runtime guards (e.g. EncryptionBadge's `=== "strict_zk"`
// check) handle hypothetical future modes that ship before the
// frontend is re-deployed.
export type EncryptionMode = "strict_zk" | "managed_encrypted";

export interface Folder {
  id: string;
  workspace_id: string;
  parent_folder_id: string | null;
  name: string;
  path: string;
  created_at: string;
  updated_at: string;
  // Encryption mode is optional in the response for legacy folder
  // rows; current folders default to "managed_encrypted".
  encryption_mode?: EncryptionMode;
}

export interface FileItem {
  id: string;
  workspace_id: string;
  folder_id: string | null;
  name: string;
  size_bytes: number;
  mime_type: string | null;
  current_version_id: string | null;
  created_at: string;
  updated_at: string;
}

export interface UploadURLResponse {
  upload_url: string;
  upload_id: string;
  object_key: string;
}

// --- Auth ----------------------------------------------------------------

export async function signup(input: {
  workspace_name: string;
  email: string;
  name: string;
  password: string;
}): Promise<AuthResponse> {
  const { data } = await client.post<AuthResponse>("/auth/signup", input);
  storeAuth(data);
  return data;
}

// LoginResponse is the discriminated union returned by POST /auth/login:
//   - normal session: AuthResponse (token, user_id, workspace_id, role).
//   - MFA-required: MFAChallengeResponse (mfa_token + must_enroll flag).
// Clients MUST inspect `mfa_required` before treating the response as
// a session token. The server intentionally returns a different shape
// in each case so that a buggy client treating the body as a
// tokenResponse fails loudly (missing `token` field) rather than
// silently storing an mfa_challenge token as a session.
export interface MFAChallengeResponse {
  mfa_required: true;
  mfa_token: string;
  expires_at: string;
  must_enroll?: boolean;
}

export type LoginResponse = AuthResponse | MFAChallengeResponse;

function isMFAChallenge(r: LoginResponse): r is MFAChallengeResponse {
  return (r as MFAChallengeResponse).mfa_required === true;
}

export async function login(input: {
  email: string;
  password: string;
}): Promise<LoginResponse> {
  const { data } = await client.post<LoginResponse>("/auth/login", input);
  if (isMFAChallenge(data)) {
    return data;
  }
  storeAuth(data);
  return data;
}

export function logout(): void {
  localStorage.removeItem(TOKEN_STORAGE_KEY);
  localStorage.removeItem(WORKSPACE_STORAGE_KEY);
  localStorage.removeItem(ROLE_STORAGE_KEY);
  localStorage.removeItem(USER_STORAGE_KEY);
}

// --- TOTP / 2FA ----------------------------------------------------------

export interface TOTPStatus {
  enabled: boolean;
  pending_enrollment: boolean;
  activated_at?: string;
  last_used_at?: string;
  recovery_codes_remaining: number;
}

export interface TOTPEnrollBeginResponse {
  secret: string;
  otpauth_uri: string;
  qr_code_png: string;
}

export interface TOTPFinalizeResponse {
  recovery_codes: string[];
}

// totpVerifyWithChallenge uses the supplied mfa challenge token
// instead of the stored session token. The challenge token is a
// short-lived (5 min) JWT marked with purpose=mfa_challenge that
// the server issues in response to a successful password login when
// the user has 2FA enrolled. Once verification succeeds the server
// returns a real session token which we store via storeAuth.
export async function totpVerifyWithChallenge(
  challengeToken: string,
  code: string,
): Promise<AuthResponse> {
  const { data } = await client.post<AuthResponse>(
    "/auth/totp/verify",
    { code },
    { headers: { Authorization: `Bearer ${challengeToken}` } },
  );
  storeAuth(data);
  return data;
}

export async function totpStatus(): Promise<TOTPStatus> {
  const { data } = await client.get<TOTPStatus>("/auth/totp/status");
  return data;
}

export async function totpEnrollBegin(): Promise<TOTPEnrollBeginResponse> {
  const { data } = await client.post<TOTPEnrollBeginResponse>(
    "/auth/totp/enroll/begin",
    {},
  );
  return data;
}

export async function totpEnrollFinalize(
  code: string,
): Promise<TOTPFinalizeResponse> {
  const { data } = await client.post<TOTPFinalizeResponse>(
    "/auth/totp/enroll/finalize",
    { code },
  );
  return data;
}

// totpEnrollBeginRequired / totpEnrollFinalizeRequired drive the
// must-enroll flow: the user logged in on a workspace that requires
// MFA but they have no credential yet. The server gave them a
// purpose=mfa_enroll token; we send it as the Authorization header
// because the dedicated `/required` routes refuse a session token
// and the AuthMiddleware-guarded `/auth/totp/enroll/*` routes refuse
// a purpose token. Two routes that converge on the same handler.
export async function totpEnrollBeginRequired(
  enrollToken: string,
): Promise<TOTPEnrollBeginResponse> {
  const { data } = await client.post<TOTPEnrollBeginResponse>(
    "/auth/totp/enroll/begin/required",
    {},
    { headers: { Authorization: `Bearer ${enrollToken}` } },
  );
  return data;
}

export async function totpEnrollFinalizeRequired(
  enrollToken: string,
  code: string,
): Promise<TOTPFinalizeResponse> {
  const { data } = await client.post<TOTPFinalizeResponse>(
    "/auth/totp/enroll/finalize/required",
    { code },
    { headers: { Authorization: `Bearer ${enrollToken}` } },
  );
  return data;
}

export async function totpDisable(password: string): Promise<void> {
  await client.post("/auth/totp/disable", { password });
}

// --- Admin: workspace MFA policy -----------------------------------------

export async function updateWorkspaceMFAPolicy(
  mfaRequired: boolean,
): Promise<{ mfa_required: boolean }> {
  const { data } = await client.patch<{ mfa_required: boolean }>(
    "/admin/workspace/mfa-policy",
    { mfa_required: mfaRequired },
  );
  return data;
}

export function currentToken(): string | null {
  return localStorage.getItem(TOKEN_STORAGE_KEY);
}

export function currentWorkspaceID(): string | null {
  return localStorage.getItem(WORKSPACE_STORAGE_KEY);
}

export function currentRole(): string | null {
  return localStorage.getItem(ROLE_STORAGE_KEY);
}

// currentUserID returns the authenticated user's UUID as stored
// at login. Used by features that need to compare the local user
// against IDs surfaced by the server (e.g. the document collab
// presence chip filters out the local user by ID rather than by
// name to avoid collisions when two users share a display name).
export function currentUserID(): string | null {
  return localStorage.getItem(USER_STORAGE_KEY);
}

function storeAuth(r: AuthResponse): void {
  localStorage.setItem(TOKEN_STORAGE_KEY, r.token);
  localStorage.setItem(WORKSPACE_STORAGE_KEY, r.workspace_id);
  if (r.user_id) {
    localStorage.setItem(USER_STORAGE_KEY, r.user_id);
  }
  if (r.role) {
    localStorage.setItem(ROLE_STORAGE_KEY, r.role);
  }
}

// --- Folders -------------------------------------------------------------

export async function listFolders(parentFolderID: string | null): Promise<Folder[]> {
  const params = parentFolderID ? { parent_folder_id: parentFolderID } : { parent_folder_id: "root" };
  const { data } = await client.get<{ folders: Folder[] }>("/folders", { params });
  return data.folders ?? [];
}

export async function getFolder(id: string): Promise<{ folder: Folder; children: Folder[] }> {
  const { data } = await client.get<{ folder: Folder; children: Folder[] }>(`/folders/${id}`);
  return data;
}

export async function createFolder(input: {
  name: string;
  parent_folder_id?: string | null;
  encryption_mode?: EncryptionMode;
}): Promise<Folder> {
  const { data } = await client.post<Folder>("/folders", input);
  return data;
}

export async function renameFolder(id: string, name: string): Promise<Folder> {
  const { data } = await client.put<Folder>(`/folders/${id}`, { name });
  return data;
}

export async function deleteFolder(id: string): Promise<void> {
  await client.delete(`/folders/${id}`);
}

// --- Documents (collab editor) ------------------------------------------

// CollabMode is the per-document feature richness knob. The folder's
// encryption_mode constrains which modes are allowed (the resolver
// lives in internal/document/capability.go and is reflected back on
// every documentResponse via the `allowed_collab_modes` field):
//
//   - "markdown"      → text + lists + headings only. Allowed in every
//                       encryption_mode (incl. strict_zk).
//   - "rich"          → tables, images, embeds. Requires the server to
//                       merge updates server-side, so managed_encrypted
//                       only.
//   - "rich_presence" → rich + Yjs awareness (live cursors / selection
//                       / online users). Same constraints as "rich".
//   - "disabled"      → tombstone state; the document exists but the
//                       WS endpoint refuses upgrades with 409. Never
//                       set by clients; surfaced as a current value
//                       when the server downgrades a doc.
export type CollabMode = "markdown" | "rich" | "rich_presence" | "disabled";

// Capability matrix is computed server-side from the folder's
// encryption_mode. Frontend uses these flags directly to gate the
// extension list and the presence chips.
export interface Capability {
  server_snapshot_allowed: boolean;
  rich_extensions_allowed: boolean;
  presence_allowed: boolean;
}

export interface Document {
  id: string;
  workspace_id: string;
  folder_id: string;
  name: string;
  collab_mode: CollabMode;
  y_state_seq_floor: number;
  snapshot_version: number;
  created_by: string;
  created_at: string;
  updated_at: string;
  deleted_at?: string | null;
  // encryption_mode is the parent folder's mode at the time of the
  // request — documents live-inherit it (see docs/PRODUCT.md §P2a).
  encryption_mode: EncryptionMode;
  // capability and allowed_collab_modes are derived from
  // encryption_mode; surfacing them on every response keeps the
  // frontend from re-implementing the resolver.
  capability: Capability;
  allowed_collab_modes: CollabMode[];
}

export async function listFolderDocuments(folderID: string): Promise<Document[]> {
  const { data } = await client.get<{ documents: Document[] }>(`/folders/${folderID}/documents`);
  return data.documents ?? [];
}

export async function createDocument(input: {
  folder_id: string;
  name: string;
  collab_mode?: CollabMode;
}): Promise<Document> {
  const { data } = await client.post<Document>("/documents", input);
  return data;
}

export async function getDocument(id: string): Promise<Document> {
  const { data } = await client.get<Document>(`/documents/${id}`);
  return data;
}

export async function renameDocument(id: string, name: string): Promise<Document> {
  const { data } = await client.put<Document>(`/documents/${id}`, { name });
  return data;
}

export async function setDocumentCollabMode(
  id: string,
  collab_mode: CollabMode,
): Promise<Document> {
  const { data } = await client.patch<Document>(`/documents/${id}/collab-mode`, { collab_mode });
  return data;
}

export async function deleteDocument(id: string): Promise<void> {
  await client.delete(`/documents/${id}`);
}

// documentCollabURL returns the absolute WebSocket URL for the
// collab endpoint of `documentID`. We resolve it against the
// browser's current location so dev (vite proxy → :8080) and prod
// (same-origin reverse proxy) both work without explicit config.
//
// Authentication on the WS surface uses the Sec-WebSocket-Protocol
// "bearer" fallback (see api/middleware/auth.go's
// WebSocketBearerSubprotocol const) because browsers cannot attach
// custom headers to a WebSocket upgrade. The caller is responsible
// for passing ["bearer", currentToken()] as the subprotocols
// argument to `new WebSocket()`.
export function documentCollabURL(documentID: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/api/documents/${documentID}/ws`;
}

// --- Files ---------------------------------------------------------------

// listFiles returns the files inside a folder. The backend exposes file
// listings via `GET /api/folders/{id}` (which returns both subfolders and
// files), so for nested folders we reuse getFolder. The workspace root
// has no such endpoint; the UI simply shows no files there and nudges
// the user to open / create a subfolder.
export async function listFiles(folderID: string | null): Promise<FileItem[]> {
  if (!folderID) return [];
  const { files } = await getFolderContents(folderID);
  return files;
}

// getFolderContents returns the full folder GET payload: folder metadata,
// subfolders, and files. Kept separate from getFolder above so pages that
// only want the folder + children don't pay for an unused files type.
export async function getFolderContents(id: string): Promise<{
  folder: Folder;
  children: Folder[];
  files: FileItem[];
}> {
  const { data } = await client.get<{
    folder: Folder;
    children: Folder[];
    files: FileItem[];
  }>(`/folders/${id}`);
  return {
    folder: data.folder,
    children: data.children ?? [],
    files: data.files ?? [],
  };
}

// requestUploadURL creates a file row on the backend AND returns a
// presigned PUT URL for its first version. This mirrors `uploadURLRequest`
// in api/drive/handler.go exactly: the backend is the authoritative source
// of the file_id (returned here as `upload_id`).
export async function requestUploadURL(input: {
  folder_id: string;
  filename: string;
  mime_type?: string;
}): Promise<UploadURLResponse> {
  const { data } = await client.post<UploadURLResponse>("/files/upload-url", input);
  return data;
}

export async function confirmUpload(input: {
  file_id: string;
  object_key: string;
  size_bytes: number;
  checksum?: string;
}): Promise<{ file: FileItem; version: unknown }> {
  const { data } = await client.post<{ file: FileItem; version: unknown }>(
    "/files/confirm-upload",
    input,
  );
  return data;
}

export async function getDownloadURL(fileID: string): Promise<string> {
  const { data } = await client.get<{ download_url: string; object_key: string }>(
    `/files/${fileID}/download-url`,
  );
  return data.download_url;
}

// getFilePreviewURL fetches a presigned GET URL for the server-rendered
// thumbnail of a file. The backend returns 404 when no preview has been
// built yet (unsupported mime or worker hasn't run); the caller renders
// a placeholder in that case.
export async function getFilePreviewURL(fileID: string): Promise<string> {
  const { data } = await client.get<{
    preview_url: string;
    object_key: string;
    mime_type: string;
  }>(`/files/${fileID}/preview-url`);
  return data.preview_url;
}

export async function deleteFile(id: string): Promise<void> {
  await client.delete(`/files/${id}`);
}

export async function renameFile(id: string, name: string): Promise<FileItem> {
  const { data } = await client.put<FileItem>(`/files/${id}`, { name });
  return data;
}

// --- Sharing -------------------------------------------------------------

// Share links and guest invites are per-resource and role-scoped. These
// mirror the JSON shapes returned by api/drive/handler.go so the
// frontend can render them without extra client-side translation.

export interface ShareLink {
  id: string;
  workspace_id: string;
  resource_type: string;
  resource_id: string;
  token: string;
  role: string;
  password_protected: boolean;
  expires_at: string | null;
  max_downloads: number | null;
  download_count: number;
  created_by: string;
  created_at: string;
  revoked_at: string | null;
}

export interface GuestInvite {
  id: string;
  workspace_id: string;
  folder_id: string;
  email: string;
  role: string;
  token?: string;
  expires_at: string | null;
  accepted_at: string | null;
  permission_id?: string;
  created_by: string;
  created_at: string;
}

export interface CreateShareLinkInput {
  resource_type: "file" | "folder";
  resource_id: string;
  role: "viewer" | "commenter" | "editor";
  password?: string;
  expires_at?: string;
  max_downloads?: number;
}

export async function createShareLink(input: CreateShareLinkInput): Promise<ShareLink> {
  const { data } = await client.post<ShareLink>("/share-links", input);
  return data;
}

// resolveShareLink hits the public endpoint that validates password /
// expiry / download cap and returns the backing resource metadata. The
// token lives in the path (not the body) so the link is copy/pasteable.
export async function resolveShareLink(
  token: string,
  password?: string,
): Promise<{ link: ShareLink; resource: Folder | FileItem }> {
  const { data } = await client.post<{ link: ShareLink; resource: Folder | FileItem }>(
    `/share-links/${encodeURIComponent(token)}`,
    password ? { password } : {},
  );
  return data;
}

export async function revokeShareLink(id: string): Promise<void> {
  await client.delete(`/share-links/${id}`);
}

// CreateGuestInviteInput mirrors api/drive/handler.go's
// createGuestInviteRequest. Guest invites are always folder-scoped —
// the backend model keeps a single folder_id column on guest_invites
// and the permission grant is issued against that folder.
export interface CreateGuestInviteInput {
  folder_id: string;
  email: string;
  role: "viewer" | "commenter" | "editor";
  expires_at?: string;
}

export async function createGuestInvite(input: CreateGuestInviteInput): Promise<GuestInvite> {
  const { data } = await client.post<GuestInvite>("/guest-invites", {
    folder_id: input.folder_id,
    email: input.email,
    role: input.role,
    expires_at: input.expires_at,
  });
  return data;
}

export async function acceptGuestInvite(id: string): Promise<GuestInvite> {
  const { data } = await client.post<GuestInvite>(`/guest-invites/${id}/accept`);
  return data;
}

export async function revokeGuestInvite(id: string): Promise<void> {
  await client.delete(`/guest-invites/${id}`);
}

// --- Search --------------------------------------------------------------

export interface SearchHit {
  type: "file" | "folder";
  id: string;
  name: string;
  path: string;
  workspace_id: string;
  folder_id: string | null;
  updated_at: string;
}

export interface SearchResponse {
  query: string;
  limit: number;
  offset: number;
  hits: SearchHit[];
}

export async function searchFiles(query: string, opts: {
  limit?: number;
  offset?: number;
} = {}): Promise<SearchResponse> {
  const { data } = await client.get<SearchResponse>("/search", {
    params: { q: query, limit: opts.limit, offset: opts.offset },
  });
  return {
    query: data.query,
    limit: data.limit,
    offset: data.offset,
    hits: data.hits ?? [],
  };
}

// --- Upload orchestration -----------------------------------------------

// uploadFile walks through the presigned-URL dance:
//   1. POST /files/upload-url  -> backend creates the file row and returns
//      { upload_url, upload_id, object_key }.
//   2. PUT the file bytes directly to upload_url.
//   3. POST /files/confirm-upload with { file_id, object_key, size_bytes }
//      to pin the new version as current.
// A null folderID is rejected because the backend requires every
// file to live under a concrete folder.
export async function uploadFile(
  file: File,
  folderID: string | null,
): Promise<FileItem> {
  if (!folderID) {
    throw new Error("cannot upload to the workspace root; open a folder first");
  }
  const upload = await requestUploadURL({
    folder_id: folderID,
    filename: file.name,
    mime_type: file.type || undefined,
  });
  const putResp = await fetch(upload.upload_url, {
    method: "PUT",
    headers: file.type ? { "Content-Type": file.type } : {},
    body: file,
  });
  if (!putResp.ok) {
    throw new Error(`upload failed: ${putResp.status}`);
  }
  const confirmed = await confirmUpload({
    file_id: upload.upload_id,
    object_key: upload.object_key,
    size_bytes: file.size,
  });
  return confirmed.file;
}

// --- Tags ---------------------------------------------------------------

export interface FileTag {
  id: string;
  file_id: string;
  workspace_id: string;
  tag: string;
  created_by: string;
  created_at: string;
}

export async function listFileTags(fileID: string): Promise<FileTag[]> {
  const { data } = await client.get<{ tags: FileTag[] }>(`/files/${fileID}/tags`);
  return data.tags ?? [];
}

export async function addFileTag(fileID: string, tag: string): Promise<FileTag> {
  const { data } = await client.post<FileTag>(`/files/${fileID}/tags`, { tag });
  return data;
}

export async function removeFileTag(fileID: string, tag: string): Promise<void> {
  await client.delete(`/files/${fileID}/tags/${encodeURIComponent(tag)}`);
}

// --- Bulk operations ----------------------------------------------------

export interface BulkResponse {
  succeeded: string[];
  failed: { id: string; error: string }[];
}

export async function bulkMove(input: {
  file_ids?: string[];
  folder_ids?: string[];
  target_folder_id: string;
}): Promise<BulkResponse> {
  const { data } = await client.post<BulkResponse>("/bulk/move", input);
  return { succeeded: data.succeeded ?? [], failed: data.failed ?? [] };
}

export async function bulkCopy(input: {
  file_ids: string[];
  target_folder_id: string;
}): Promise<BulkResponse> {
  const { data } = await client.post<BulkResponse>("/bulk/copy", input);
  return { succeeded: data.succeeded ?? [], failed: data.failed ?? [] };
}

export async function bulkDelete(input: {
  file_ids?: string[];
  folder_ids?: string[];
}): Promise<BulkResponse> {
  const { data } = await client.post<BulkResponse>("/bulk/delete", input);
  return { succeeded: data.succeeded ?? [], failed: data.failed ?? [] };
}

export async function bulkDownload(fileIDs: string[]): Promise<Blob> {
  const { data } = await client.post<Blob>(
    "/bulk/download",
    { file_ids: fileIDs },
    { responseType: "blob" },
  );
  return data;
}

// --- Admin --------------------------------------------------------------

export interface AdminUser {
  id: string;
  email: string;
  name: string;
  role: string;
  workspace_id: string;
  deactivated_at: string | null;
  created_at: string;
}

export interface AuditEntry {
  id: string;
  workspace_id: string;
  actor_id?: string | null;
  action: string;
  resource_type?: string | null;
  resource_id?: string | null;
  ip_address?: string | null;
  user_agent?: string | null;
  metadata?: unknown;
  created_at: string;
}

export interface StorageUsage {
  total_bytes: number;
  per_user: {
    user_id: string;
    email: string;
    total_bytes: number;
    file_count: number;
  }[];
}

export interface RetentionPolicy {
  id: string;
  workspace_id: string;
  folder_id: string | null;
  max_versions?: number | null;
  max_age_days?: number | null;
  archive_after_days?: number | null;
  created_at: string;
  updated_at: string;
}

export async function fetchUsers(): Promise<AdminUser[]> {
  const { data } = await client.get<{ users: AdminUser[] }>("/admin/users");
  return data.users ?? [];
}

export async function inviteUser(input: {
  email: string;
  name: string;
  password: string;
  role: string;
}): Promise<AdminUser> {
  const { data } = await client.post<AdminUser>("/admin/users", input);
  return data;
}

export async function deactivateUser(id: string): Promise<void> {
  await client.delete(`/admin/users/${id}`);
}

export async function updateUserRole(id: string, role: string): Promise<AdminUser> {
  const { data } = await client.put<AdminUser>(`/admin/users/${id}/role`, { role });
  return data;
}

export async function fetchAuditLog(opts: {
  action?: string;
  limit?: number;
  offset?: number;
} = {}): Promise<AuditEntry[]> {
  const { data } = await client.get<{ entries: AuditEntry[] }>("/admin/audit-log", {
    params: { action: opts.action, limit: opts.limit, offset: opts.offset },
  });
  return data.entries ?? [];
}

export async function fetchStorageUsage(): Promise<StorageUsage> {
  const { data } = await client.get<StorageUsage>("/admin/storage-usage");
  return data;
}

export async function fetchRetentionPolicies(): Promise<RetentionPolicy[]> {
  const { data } = await client.get<{ policies: RetentionPolicy[] }>("/admin/retention-policies");
  return data.policies ?? [];
}

export async function upsertRetentionPolicy(input: {
  folder_id?: string | null;
  max_versions?: number | null;
  max_age_days?: number | null;
  archive_after_days?: number | null;
}): Promise<RetentionPolicy> {
  const { data } = await client.post<RetentionPolicy>("/admin/retention-policies", input);
  return data;
}

export async function deleteRetentionPolicy(id: string): Promise<void> {
  await client.delete(`/admin/retention-policies/${id}`);
}

// --- Placement -----------------------------------------------------

// PlacementPolicy mirrors internal/fabric.Policy. Only the subset the
// admin UI actually edits is modelled; other fields (tenant, cache
// location) round-trip through JSON unchanged on PUT because we send
// the entire payload we received on GET.
export interface PlacementPolicy {
  tenant?: string;
  bucket?: string;
  policy: {
    encryption: {
      mode: string;
      kms?: string;
    };
    placement: {
      provider: string[];
      region?: string[];
      country?: string[];
      storage_class?: string[];
      cache_location?: string;
    };
  };
}

export async function fetchPlacement(): Promise<PlacementPolicy> {
  const { data } = await client.get<PlacementPolicy>("/admin/placement");
  return data;
}

export async function updatePlacement(policy: PlacementPolicy): Promise<void> {
  await client.put("/admin/placement", policy);
}

// --- Customer-managed keys -----------------------------------------

export async function fetchCMK(): Promise<{ cmk_uri: string }> {
  const { data } = await client.get<{ cmk_uri: string }>("/admin/cmk");
  return data;
}

export async function updateCMK(cmk_uri: string): Promise<void> {
  await client.put("/admin/cmk", { cmk_uri });
}

// --- KChat rooms ---------------------------------------------------

export interface KChatRoom {
  id: string;
  workspace_id: string;
  kchat_room_id: string;
  folder_id: string;
  created_by: string;
  created_at: string;
}

export interface KChatMemberSync {
  user_id: string;
  role: string;
}

export async function fetchKChatRooms(): Promise<KChatRoom[]> {
  const { data } = await client.get<{ rooms: KChatRoom[] }>("/kchat/rooms");
  return data.rooms ?? [];
}

export async function createKChatRoom(kchat_room_id: string): Promise<KChatRoom> {
  const { data } = await client.post<KChatRoom>("/kchat/rooms", { kchat_room_id });
  return data;
}

export async function deleteKChatRoom(id: string): Promise<void> {
  await client.delete(`/kchat/rooms/${id}`);
}

export async function syncKChatMembers(
  id: string,
  members: KChatMemberSync[],
): Promise<{ synced: number }> {
  const { data } = await client.post<{ synced: number }>(
    `/kchat/rooms/${id}/sync-members`,
    { members },
  );
  return data;
}

// --- Client-room templates -----------------------------------------

export interface ClientRoomTemplate {
  name: string;
  sub_folders: string[];
}

export async function fetchClientRoomTemplates(): Promise<ClientRoomTemplate[]> {
  const { data } = await client.get<{ templates: ClientRoomTemplate[] }>(
    "/client-rooms/templates",
  );
  return data.templates ?? [];
}

export interface ClientRoomFromTemplateResult {
  id: string;
  folder_id: string;
  name: string;
  share_link_token: string;
  sub_folder_ids: string[];
}

export async function createClientRoomFromTemplate(
  template: string,
  client_name: string,
): Promise<ClientRoomFromTemplateResult> {
  const { data } = await client.post<ClientRoomFromTemplateResult>(
    "/client-rooms/from-template",
    { template, name: client_name },
  );
  return data;
}

// --- Billing ------------------------------------------------------------

export interface BillingUsageSummary {
  tier: string;
  storage_used_bytes: number;
  storage_limit_bytes: number;
  bandwidth_used_bytes_month: number;
  bandwidth_limit_bytes_month: number;
  user_count: number;
  user_limit: number;
  plan_configured: boolean;
}

export async function fetchBillingUsage(): Promise<BillingUsageSummary> {
  const { data } = await client.get<BillingUsageSummary>("/admin/billing/usage");
  return data;
}

export async function updateBillingPlan(input: {
  tier: string;
  max_storage_bytes?: number;
  max_users?: number;
  max_bandwidth_bytes_monthly?: number;
}): Promise<unknown> {
  const { data } = await client.put("/admin/billing/plan", input);
  return data;
}

// Stripe Checkout / Customer Portal --------------------------------------
//
// Both endpoints return a Stripe-hosted URL that the caller is expected
// to send the browser to (window.location.assign). The backend creates
// the session on Stripe's side and tags it with `metadata.workspace_id`
// + `metadata.tier` so the webhook handler can resolve the plan when
// Checkout completes.

export interface StripeSessionResponse {
  url: string;
}

export async function createCheckoutSession(input: {
  tier: string;
  success_url: string;
  cancel_url: string;
}): Promise<StripeSessionResponse> {
  const { data } = await client.post<StripeSessionResponse>(
    "/admin/billing/checkout-session",
    input,
  );
  return data;
}

export async function createPortalSession(input: {
  return_url: string;
}): Promise<StripeSessionResponse> {
  const { data } = await client.post<StripeSessionResponse>(
    "/admin/billing/portal-session",
    input,
  );
  return data;
}

export default client;
