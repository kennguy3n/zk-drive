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

// Redirect to /login on 401 so expired sessions don't leave the UI stuck.
client.interceptors.response.use(
  (resp) => resp,
  (err) => {
    if (err?.response?.status === 401) {
      localStorage.removeItem(TOKEN_STORAGE_KEY);
      localStorage.removeItem(WORKSPACE_STORAGE_KEY);
      if (window.location.pathname !== "/login") {
        window.location.href = "/login";
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

export interface Folder {
  id: string;
  workspace_id: string;
  parent_folder_id: string | null;
  name: string;
  path: string;
  created_at: string;
  updated_at: string;
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

export async function login(input: {
  email: string;
  password: string;
}): Promise<AuthResponse> {
  const { data } = await client.post<AuthResponse>("/auth/login", input);
  storeAuth(data);
  return data;
}

export function logout(): void {
  localStorage.removeItem(TOKEN_STORAGE_KEY);
  localStorage.removeItem(WORKSPACE_STORAGE_KEY);
  localStorage.removeItem(ROLE_STORAGE_KEY);
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

function storeAuth(r: AuthResponse): void {
  localStorage.setItem(TOKEN_STORAGE_KEY, r.token);
  localStorage.setItem(WORKSPACE_STORAGE_KEY, r.workspace_id);
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

// --- Files ---------------------------------------------------------------

// listFiles returns the files inside a folder. The backend exposes file
// listings via `GET /api/folders/{id}` (which returns both subfolders and
// files), so for nested folders we reuse getFolder. The root has no such
// endpoint in Phase 1; the UI simply shows no files there and nudges the
// user to open / create a subfolder.
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
// A null folderID is rejected because the Phase 1 backend requires every
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

export default client;
