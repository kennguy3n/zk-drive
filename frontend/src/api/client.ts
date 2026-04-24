import axios, { type AxiosInstance } from "axios";

// Shared Axios instance pointed at the dev proxy (/api -> :8080). All
// request/response types below match the Go handler JSON.
const client: AxiosInstance = axios.create({
  baseURL: "/api",
  headers: { "Content-Type": "application/json" },
});

const TOKEN_STORAGE_KEY = "zkdrive.token";
const WORKSPACE_STORAGE_KEY = "zkdrive.workspace_id";

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
}

export function currentToken(): string | null {
  return localStorage.getItem(TOKEN_STORAGE_KEY);
}

export function currentWorkspaceID(): string | null {
  return localStorage.getItem(WORKSPACE_STORAGE_KEY);
}

function storeAuth(r: AuthResponse): void {
  localStorage.setItem(TOKEN_STORAGE_KEY, r.token);
  localStorage.setItem(WORKSPACE_STORAGE_KEY, r.workspace_id);
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

export async function deleteFile(id: string): Promise<void> {
  await client.delete(`/files/${id}`);
}

export async function renameFile(id: string, name: string): Promise<FileItem> {
  const { data } = await client.put<FileItem>(`/files/${id}`, { name });
  return data;
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

export default client;
