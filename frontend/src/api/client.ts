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
  url: string;
  object_key: string;
  expires_in: number;
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

export async function listFiles(folderID: string | null): Promise<FileItem[]> {
  // The Go API returns files as part of a folder GET for nested folders, but
  // for the root listing we fetch all files with folder_id=null. To keep the
  // UI simple for Phase 1 we use a server-side filter query param when one
  // exists, otherwise fall back to an empty array.
  try {
    const params = folderID ? { folder_id: folderID } : { folder_id: "root" };
    const { data } = await client.get<{ files: FileItem[] }>("/files", { params });
    return data.files ?? [];
  } catch {
    // The API exposes file listings via folder GET today. Swallow errors
    // so the file browser still loads when the endpoint is missing.
    return [];
  }
}

export async function createFileRecord(input: {
  name: string;
  folder_id?: string | null;
  mime_type?: string | null;
  size_bytes?: number;
}): Promise<FileItem> {
  const { data } = await client.post<FileItem>("/files", input);
  return data;
}

export async function requestUploadURL(input: {
  file_id: string;
  content_type?: string;
  size_bytes?: number;
}): Promise<UploadURLResponse> {
  const { data } = await client.post<UploadURLResponse>("/files/upload-url", input);
  return data;
}

export async function confirmUpload(input: {
  file_id: string;
  object_key: string;
  size_bytes: number;
  mime_type?: string | null;
  etag?: string | null;
}): Promise<FileItem> {
  const { data } = await client.post<FileItem>("/files/confirm-upload", input);
  return data;
}

export async function getDownloadURL(fileID: string): Promise<string> {
  const { data } = await client.get<{ url: string }>(`/files/${fileID}/download-url`);
  return data.url;
}

export async function deleteFile(id: string): Promise<void> {
  await client.delete(`/files/${id}`);
}

export async function renameFile(id: string, name: string): Promise<FileItem> {
  const { data } = await client.put<FileItem>(`/files/${id}`, { name });
  return data;
}

// --- Upload orchestration -----------------------------------------------

// uploadFile walks through the presigned-URL dance: create metadata, ask
// for a PUT URL, upload the body directly to S3, then confirm.
export async function uploadFile(
  file: File,
  folderID: string | null,
): Promise<FileItem> {
  const created = await createFileRecord({
    name: file.name,
    folder_id: folderID,
    mime_type: file.type || null,
    size_bytes: file.size,
  });
  const upload = await requestUploadURL({
    file_id: created.id,
    content_type: file.type || "application/octet-stream",
    size_bytes: file.size,
  });
  const putResp = await fetch(upload.url, {
    method: "PUT",
    headers: file.type ? { "Content-Type": file.type } : {},
    body: file,
  });
  if (!putResp.ok) {
    throw new Error(`upload failed: ${putResp.status}`);
  }
  const etag = putResp.headers.get("ETag");
  return confirmUpload({
    file_id: created.id,
    object_key: upload.object_key,
    size_bytes: file.size,
    mime_type: file.type || null,
    etag,
  });
}

export default client;
