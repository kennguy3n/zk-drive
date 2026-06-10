import axios, { type AxiosInstance } from "axios";
import { is401SoftFailure } from "./auth401";

// Shared Axios instance pointed at the dev proxy (/api -> :8080). All
// request/response types below match the Go handler JSON.
const client: AxiosInstance = axios.create({
  baseURL: "/api",
  headers: { "Content-Type": "application/json" },
});

// pushTeardownClient is a bare Axios instance with NO request/response
// interceptors, used solely for the best-effort DELETE /push/subscribe
// in tearDownPushSubscription. Teardown is frequently triggered BY a
// 401 (forced session death), and the captured token is already
// expired, so routing that DELETE through the shared `client` would
// return 401 and re-enter the 401 response interceptor — a needless
// extra request and a duplicate teardown. The (stale) token is attached
// explicitly as an Authorization header on each call instead.
const pushTeardownClient: AxiosInstance = axios.create({
  baseURL: "/api",
  headers: { "Content-Type": "application/json" },
});

const TOKEN_STORAGE_KEY = "zkdrive.token";
const WORKSPACE_STORAGE_KEY = "zkdrive.workspace_id";
const ROLE_STORAGE_KEY = "zkdrive.role";
const USER_STORAGE_KEY = "zkdrive.user_id";

// AUTH_CHANGE_EVENT is dispatched on `window` whenever auth state is
// mutated in THIS tab (login / signup / MFA verify / logout). The
// browser's native "storage" event only fires in OTHER tabs, so a
// long-lived subscriber mounted before login (e.g. useAuth at the App
// root) never learns about a same-tab login without this. useAuth
// listens for both events to stay in sync regardless of which tab
// changed the session.
export const AUTH_CHANGE_EVENT = "zkdrive:auth-change";

function emitAuthChange(): void {
  if (typeof window !== "undefined") {
    window.dispatchEvent(new Event(AUTH_CHANGE_EVENT));
  }
}

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

// Redirect to /login on session-expiry 401s so stale sessions don't
// leave the UI stuck. Clear ALL auth-derived localStorage keys (token,
// workspace, user_id) so the next login is a clean slate; otherwise a
// stale user_id could persist into a different user's session and break
// presence (cursor colors keyed on the wrong id, PresenceChips failing
// to filter the local user, etc.).
//
// We treat 401 as "session expired" UNLESS the structured error code
// indicates a soft auth failure that the calling page should handle.
// The classification lives in ./auth401 (NON_SESSION_401_CODES vs
// SESSION_DEAD_401_CODES); a regression test there (auth401.test.ts)
// asserts every 401-emitting backend code is explicitly classified
// in exactly one bucket, so a contributor adding a new 401 path
// can't accidentally land on the wrong side of the carve-out.
client.interceptors.response.use(
  (resp) => resp,
  (err) => {
    if (err?.response?.status === 401) {
      const data = err.response.data as { code?: string } | undefined;
      const code = typeof data?.code === "string" ? data.code : null;
      if (!is401SoftFailure(code)) {
        // Mirror logout(): tear down the browser push subscription so a
        // forcibly-expired/revoked session stops receiving pushes that
        // can carry workspace-sensitive content. Capture the (now-stale)
        // token before clearing storage — the server DELETE may itself
        // 401 and is swallowed, but the browser-side unsubscribe() always
        // runs and stops delivery, and the server row is auto-pruned on
        // the next 410. Without this, only an explicit logout() cleaned
        // up; an idle-timeout / server-side revocation left the sub live.
        tearDownPushSubscription(currentToken());
        localStorage.removeItem(TOKEN_STORAGE_KEY);
        localStorage.removeItem(WORKSPACE_STORAGE_KEY);
        localStorage.removeItem(ROLE_STORAGE_KEY);
        localStorage.removeItem(USER_STORAGE_KEY);
        // Notify same-tab useAuth consumers that the session ended, for
        // consistency with logout()/storeAuth(). The redirect below
        // resets React state via full reload in the common case, but
        // when already on /login that redirect is skipped, so without
        // this event useAuth would keep stale state until the next
        // unrelated render.
        emitAuthChange();
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
  // Tear down the browser push subscription (and its server-side row)
  // before clearing the token. Push notifications can carry workspace-
  // sensitive content (file names, share links), so a browser that is
  // no longer authenticated must stop receiving them. currentToken() is
  // captured now because the server-side DELETE needs the Authorization
  // header and the token is removed synchronously just below.
  // Best-effort and fire-and-forget — never blocks logout.
  tearDownPushSubscription(currentToken());
  localStorage.removeItem(TOKEN_STORAGE_KEY);
  localStorage.removeItem(WORKSPACE_STORAGE_KEY);
  localStorage.removeItem(ROLE_STORAGE_KEY);
  localStorage.removeItem(USER_STORAGE_KEY);
  emitAuthChange();
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
  emitAuthChange();
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

// --- ONLYOFFICE office editing -------------------------------------------

// OnlyOfficeEditorConfig mirrors the JSON the backend
// (api/drive/onlyoffice_handler.go → internal/collab.EditorConfig)
// hands to `new DocsAPI.DocEditor(...)`. documentServerUrl tells the
// frontend which Document Server's api.js to load; the nested
// document / editorConfig blocks are passed through to DocsAPI
// verbatim, and token is the HS256 JWT the Document Server validates.
export interface OnlyOfficeEditorConfig {
  documentServerUrl: string;
  documentType: string;
  document: {
    title: string;
    url: string;
    fileType: string;
    key: string;
    permissions: { edit: boolean; download: boolean; print: boolean };
  };
  editorConfig: {
    mode: string;
    callbackUrl: string;
    lang?: string;
    user: { id: string; name: string };
  };
  token?: string;
}

// getOnlyOfficeStatus reports whether collaborative office editing is
// configured server-side (ONLYOFFICE_URL set). The frontend uses it to
// gate the "Open in Editor" affordance so it stays hidden in
// deployments without a Document Server.
export async function getOnlyOfficeStatus(): Promise<boolean> {
  const { data } = await client.get<{ enabled: boolean }>("/onlyoffice/status");
  return data.enabled;
}

// getEditorConfig fetches the signed ONLYOFFICE editor config for a
// file. mode is "edit" (default) or "view"; the server downgrades an
// "edit" request to "view" when the caller lacks editor access.
export async function getEditorConfig(
  fileID: string,
  mode: "edit" | "view" = "edit",
): Promise<OnlyOfficeEditorConfig> {
  const { data } = await client.get<OnlyOfficeEditorConfig>(
    `/files/${fileID}/editor-config`,
    { params: { mode } },
  );
  return data;
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

// suggestFileTags fetches AI-suggested tags for a file. The
// suggestions are advisory — the caller is expected to render them
// as clickable chips and pipe a selection through addFileTag, so
// the LLM never writes tags directly. A 409 means the file lives in
// a strict-ZK folder (server has no plaintext); a 501 means the
// suggestion service hasn't been wired in this deployment. Both
// are surfaced as a thrown axios error so the calling component
// can decide whether to hide the affordance.
export async function suggestFileTags(fileID: string): Promise<string[]> {
  const { data } = await client.get<{ suggestions: string[] }>(
    `/files/${fileID}/tag-suggestions`,
  );
  return data.suggestions ?? [];
}

export interface SearchExpansion {
  query: string;
  terms: string[];
  llm_used: boolean;
  language: string;
}

// expandSearchQuery requests synonym / related-term suggestions for
// a search query. The frontend renders the terms as chips next to
// the search bar; selecting one re-issues searchFiles with the
// expanded term. A 501 means the expansion service isn't wired —
// the search bar should hide the expansion strip silently in that
// case (no toast, no error UI).
export async function expandSearchQuery(query: string): Promise<SearchExpansion> {
  const { data } = await client.get<SearchExpansion>("/search/expand", {
    params: { q: query },
  });
  return {
    query: data.query,
    terms: data.terms ?? [],
    llm_used: !!data.llm_used,
    language: data.language ?? "",
  };
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

// --- Health dashboard (WS8 8.1) ------------------------------------

// HealthColor mirrors internal/health.Color: the traffic-light signal
// rendered as a coloured pill per subsystem. "unknown" means the
// subsystem is not configured in this deployment and is excluded from
// the overall roll-up (rendered grey).
export type HealthColor = "green" | "yellow" | "red" | "unknown";

// HealthSubsystem mirrors internal/health.Subsystem. detail is an
// opaque, subsystem-specific bag of structured context (pool stats,
// memory usage, stream depths, worker last-seen, …) rendered as
// key/value rows; error is a short, non-sensitive failure summary
// present only when status is red/yellow.
export interface HealthSubsystem {
  name: string;
  status: HealthColor;
  detail?: Record<string, unknown>;
  error?: string;
}

// HealthReport mirrors internal/health.Report, the body of
// GET /api/admin/health-dashboard.
export interface HealthReport {
  status: HealthColor;
  generated_at: string;
  subsystems: HealthSubsystem[];
}

export async function fetchHealthDashboard(): Promise<HealthReport> {
  const { data } = await client.get<HealthReport>("/admin/health-dashboard");
  return data;
}

// --- Guided setup wizard (WS8 8.2) ---------------------------------

// SetupStep mirrors internal/setup.Step.
export interface SetupStep {
  configured: boolean;
  detail?: string;
}

// SetupOptionalServices mirrors internal/setup.OptionalServices.
export interface SetupOptionalServices {
  email: boolean;
  virus_scanning: boolean;
  ai: boolean;
  collaborative_editing: boolean;
}

// SetupSteps mirrors internal/setup.Steps. Present only while setup is
// incomplete (the backend omits it once complete to avoid leaking the
// deployment shape to anonymous callers).
export interface SetupSteps {
  admin_account: SetupStep;
  storage: SetupStep;
  workspace: SetupStep;
  optional_services: SetupOptionalServices;
}

// SetupStatus mirrors internal/setup.Status, the body of
// GET /api/setup/status.
export interface SetupStatus {
  setup_completed: boolean;
  needs_setup: boolean;
  completed_at?: string;
  steps?: SetupSteps;
}

// fetchSetupStatus reads the setup status. It is intentionally callable
// pre-authentication (the wizard runs before any admin exists). It goes
// through the shared `client`, but that is safe before login: the
// request interceptor simply omits the Authorization header when no
// token is stored, and GET /api/setup/status is a public endpoint that
// answers 200 to anonymous callers — so the 401 session-teardown
// response interceptor is never reached.
export async function fetchSetupStatus(): Promise<SetupStatus> {
  const { data } = await client.get<SetupStatus>("/setup/status");
  return data;
}

export interface TestStorageInput {
  endpoint: string;
  bucket: string;
  access_key: string;
  secret_key: string;
  region?: string;
}

export interface TestStorageResult {
  ok: boolean;
  error?: string;
}

// testSetupStorage validates S3/Fabric credentials via a real
// HeadBucket before the operator commits them. A failed connection is
// reported as { ok: false, error } with HTTP 200, so callers render an
// inline message rather than treating it as a request failure.
export async function testSetupStorage(input: TestStorageInput): Promise<TestStorageResult> {
  const { data } = await client.post<TestStorageResult>("/setup/test-storage", input);
  return data;
}

// completeSetup marks the wizard finished (admin-only). Idempotent.
export async function completeSetup(): Promise<void> {
  await client.post("/setup/complete", {});
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

// ---------------------------------------------------------------------------
// Web Push (RFC 8030 + VAPID) subscription management. The server signs
// push messages with a VAPID key pair; the frontend fetches the public key,
// subscribes via the browser PushManager, and registers the resulting
// PushSubscription here so the server can deliver notifications while no
// tab / WebSocket is connected. When VAPID keys are unconfigured the server
// responds 501 and these helpers surface that to the caller.

export async function getVapidPublicKey(): Promise<string> {
  const { data } = await client.get<{ public_key: string }>("/push/vapid-public-key");
  return data.public_key;
}

// registerPushSubscription POSTs a browser PushSubscription (its toJSON()
// shape: { endpoint, keys: { p256dh, auth } }) to the server.
export async function registerPushSubscription(sub: PushSubscriptionJSON): Promise<void> {
  await client.post("/push/subscribe", {
    endpoint: sub.endpoint,
    keys: {
      p256dh: sub.keys?.p256dh ?? "",
      auth: sub.keys?.auth ?? "",
    },
  });
}

export async function unregisterPushSubscription(endpoint: string): Promise<void> {
  await client.delete("/push/subscribe", { data: { endpoint } });
}

// tearDownPushSubscription removes the browser PushSubscription and its
// server-side row on logout (and on forced 401 session death) so a
// browser that is no longer authenticated stops receiving push
// notifications. The server DELETE needs the auth token, which the
// caller captures *before* clearing localStorage and passes here; it is
// sent as an explicit Authorization header on pushTeardownClient — a
// bare instance with no interceptors — so a stale token neither relies
// on the request interceptor (which would read the already-cleared
// token) nor re-enters the 401 response interceptor on rejection.
// Entirely best-effort: push being unsupported, no active subscription,
// or a network failure is swallowed so logout always completes.
export function tearDownPushSubscription(token: string | null): void {
  if (
    typeof window === "undefined" ||
    !("serviceWorker" in navigator) ||
    !("PushManager" in window)
  ) {
    return;
  }
  void navigator.serviceWorker.ready
    .then((registration) => registration.pushManager.getSubscription())
    .then(async (subscription) => {
      if (!subscription) {
        return;
      }
      if (token) {
        await pushTeardownClient
          .delete("/push/subscribe", {
            data: { endpoint: subscription.endpoint },
            headers: { Authorization: `Bearer ${token}` },
          })
          .catch(() => undefined);
      }
      await subscription.unsubscribe().catch(() => undefined);
    })
    .catch(() => undefined);
}

export default client;
