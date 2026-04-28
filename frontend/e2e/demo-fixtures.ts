// frontend/e2e/demo-fixtures.ts
// Realistic mock data for demo screenshots. Shapes match
// frontend/src/api/client.ts interfaces. Every API the SPA calls during a
// demo flow is intercepted via page.route() so screenshots can be
// generated without a running backend.

import type { Page } from "@playwright/test";

// ---- IDs ----
const WS_ID = "aaaaaaaa-1111-2222-3333-444444444444";
const USER_ADMIN_ID = "bbbbbbbb-1111-2222-3333-444444444444";
const USER_MEMBER1_ID = "cccccccc-1111-2222-3333-444444444444";
const USER_MEMBER2_ID = "dddddddd-1111-2222-3333-444444444444";
const USER_MEMBER3_ID = "eeeeeeee-1111-2222-3333-444444444444";

export const FOLDER_ENGINEERING_ID = "f0000001-0000-0000-0000-000000000001";
export const FOLDER_LEGAL_ID = "f0000002-0000-0000-0000-000000000002";
export const FOLDER_FINANCE_ID = "f0000003-0000-0000-0000-000000000003";
export const FOLDER_VAULT_ID = "f0000004-0000-0000-0000-000000000004";
export const FOLDER_MARKETING_ID = "f0000005-0000-0000-0000-000000000005";
const FOLDER_BACKEND_ID = "f0000006-0000-0000-0000-000000000006";
const FOLDER_FRONTEND_ID = "f0000007-0000-0000-0000-000000000007";

const FILE1_ID = "a1000001-0000-0000-0000-000000000001";
const FILE2_ID = "a1000002-0000-0000-0000-000000000002";
const FILE3_ID = "a1000003-0000-0000-0000-000000000003";
const FILE4_ID = "a1000004-0000-0000-0000-000000000004";
const FILE5_ID = "a1000005-0000-0000-0000-000000000005";

const now = new Date().toISOString();
const yesterday = new Date(Date.now() - 86400000).toISOString();
const twoDaysAgo = new Date(Date.now() - 2 * 86400000).toISOString();
const weekAgo = new Date(Date.now() - 7 * 86400000).toISOString();

// ---- Root folders ----
export const ROOT_FOLDERS = [
  { id: FOLDER_ENGINEERING_ID, workspace_id: WS_ID, parent_folder_id: null, name: "Engineering", path: "/Engineering/", encryption_mode: "managed_encrypted", created_at: weekAgo, updated_at: yesterday },
  { id: FOLDER_LEGAL_ID, workspace_id: WS_ID, parent_folder_id: null, name: "Legal Contracts", path: "/Legal Contracts/", encryption_mode: "strict_zk", created_at: weekAgo, updated_at: twoDaysAgo },
  { id: FOLDER_FINANCE_ID, workspace_id: WS_ID, parent_folder_id: null, name: "Finance", path: "/Finance/", encryption_mode: "managed_encrypted", created_at: weekAgo, updated_at: now },
  { id: FOLDER_VAULT_ID, workspace_id: WS_ID, parent_folder_id: null, name: "Client Vault", path: "/Client Vault/", encryption_mode: "strict_zk", created_at: twoDaysAgo, updated_at: yesterday },
  { id: FOLDER_MARKETING_ID, workspace_id: WS_ID, parent_folder_id: null, name: "Marketing Assets", path: "/Marketing Assets/", encryption_mode: "managed_encrypted", created_at: weekAgo, updated_at: now },
];

// ---- Engineering subfolders ----
export const ENGINEERING_CHILDREN = [
  { id: FOLDER_BACKEND_ID, workspace_id: WS_ID, parent_folder_id: FOLDER_ENGINEERING_ID, name: "Backend", path: "/Engineering/Backend/", encryption_mode: "managed_encrypted", created_at: weekAgo, updated_at: yesterday },
  { id: FOLDER_FRONTEND_ID, workspace_id: WS_ID, parent_folder_id: FOLDER_ENGINEERING_ID, name: "Frontend", path: "/Engineering/Frontend/", encryption_mode: "managed_encrypted", created_at: weekAgo, updated_at: twoDaysAgo },
];

// ---- Files inside Engineering ----
export const ENGINEERING_FILES = [
  { id: FILE1_ID, workspace_id: WS_ID, folder_id: FOLDER_ENGINEERING_ID, name: "architecture-overview.pdf", size_bytes: 2_450_000, mime_type: "application/pdf", current_version_id: "v001", created_at: weekAgo, updated_at: yesterday },
  { id: FILE2_ID, workspace_id: WS_ID, folder_id: FOLDER_ENGINEERING_ID, name: "api-spec-v3.yaml", size_bytes: 48_200, mime_type: "application/x-yaml", current_version_id: "v002", created_at: twoDaysAgo, updated_at: twoDaysAgo },
  { id: FILE3_ID, workspace_id: WS_ID, folder_id: FOLDER_ENGINEERING_ID, name: "deployment-runbook.md", size_bytes: 12_800, mime_type: "text/markdown", current_version_id: "v003", created_at: weekAgo, updated_at: now },
  { id: FILE4_ID, workspace_id: WS_ID, folder_id: FOLDER_ENGINEERING_ID, name: "load-test-results.csv", size_bytes: 890_000, mime_type: "text/csv", current_version_id: "v004", created_at: yesterday, updated_at: yesterday },
  { id: FILE5_ID, workspace_id: WS_ID, folder_id: FOLDER_ENGINEERING_ID, name: "security-audit-2026.pdf", size_bytes: 5_120_000, mime_type: "application/pdf", current_version_id: "v005", created_at: twoDaysAgo, updated_at: yesterday },
];

// ---- Admin: Users ----
export const ADMIN_USERS = {
  users: [
    { id: USER_ADMIN_ID, email: "alice@acmecorp.com", name: "Alice Chen", role: "admin", workspace_id: WS_ID, deactivated_at: null, created_at: weekAgo },
    { id: USER_MEMBER1_ID, email: "bob@acmecorp.com", name: "Bob Martinez", role: "member", workspace_id: WS_ID, deactivated_at: null, created_at: weekAgo },
    { id: USER_MEMBER2_ID, email: "carol@acmecorp.com", name: "Carol Nguyen", role: "member", workspace_id: WS_ID, deactivated_at: null, created_at: twoDaysAgo },
    { id: USER_MEMBER3_ID, email: "dave@acmecorp.com", name: "Dave Patel", role: "admin", workspace_id: WS_ID, deactivated_at: null, created_at: yesterday },
    { id: "ff000001-0000-0000-0000-000000000001", email: "eve@acmecorp.com", name: "Eve Thompson", role: "member", workspace_id: WS_ID, deactivated_at: twoDaysAgo, created_at: weekAgo },
  ],
};

// ---- Admin: Audit log ----
export const AUDIT_LOG = {
  entries: [
    { id: "audit-001", workspace_id: WS_ID, actor_id: USER_ADMIN_ID, action: "auth.signup", resource_type: "workspace", resource_id: WS_ID, ip_address: "203.0.113.42", user_agent: "Chrome/125", metadata: null, created_at: weekAgo },
    { id: "audit-002", workspace_id: WS_ID, actor_id: USER_ADMIN_ID, action: "folder.create", resource_type: "folder", resource_id: FOLDER_ENGINEERING_ID, ip_address: "203.0.113.42", user_agent: "Chrome/125", metadata: null, created_at: weekAgo },
    { id: "audit-003", workspace_id: WS_ID, actor_id: USER_MEMBER1_ID, action: "file.upload", resource_type: "file", resource_id: FILE1_ID, ip_address: "198.51.100.7", user_agent: "Firefox/126", metadata: null, created_at: twoDaysAgo },
    { id: "audit-004", workspace_id: WS_ID, actor_id: USER_ADMIN_ID, action: "share_link.create", resource_type: "folder", resource_id: FOLDER_LEGAL_ID, ip_address: "203.0.113.42", user_agent: "Chrome/125", metadata: null, created_at: twoDaysAgo },
    { id: "audit-005", workspace_id: WS_ID, actor_id: USER_MEMBER2_ID, action: "file.upload", resource_type: "file", resource_id: FILE3_ID, ip_address: "192.0.2.15", user_agent: "Safari/17", metadata: null, created_at: yesterday },
    { id: "audit-006", workspace_id: WS_ID, actor_id: USER_ADMIN_ID, action: "user.invite", resource_type: "user", resource_id: USER_MEMBER3_ID, ip_address: "203.0.113.42", user_agent: "Chrome/125", metadata: null, created_at: yesterday },
    { id: "audit-007", workspace_id: WS_ID, actor_id: USER_MEMBER1_ID, action: "file.download", resource_type: "file", resource_id: FILE5_ID, ip_address: "198.51.100.7", user_agent: "Firefox/126", metadata: null, created_at: now },
    { id: "audit-008", workspace_id: WS_ID, actor_id: USER_MEMBER3_ID, action: "folder.create", resource_type: "folder", resource_id: FOLDER_VAULT_ID, ip_address: "10.0.0.5", user_agent: "Chrome/125", metadata: null, created_at: now },
  ],
};

// ---- Admin: Retention policies ----
export const RETENTION_POLICIES = {
  policies: [
    { id: "ret-001", workspace_id: WS_ID, folder_id: null, max_versions: 10, max_age_days: 90, archive_after_days: null, created_at: weekAgo, updated_at: weekAgo },
    { id: "ret-002", workspace_id: WS_ID, folder_id: FOLDER_LEGAL_ID, max_versions: null, max_age_days: 365, archive_after_days: 180, created_at: twoDaysAgo, updated_at: twoDaysAgo },
  ],
};

// ---- Admin: Storage usage ----
export const STORAGE_USAGE = {
  total_bytes: 15_728_640_000, // ~14.6 GB
  per_user: [
    { user_id: USER_ADMIN_ID, email: "alice@acmecorp.com", total_bytes: 6_442_450_944, file_count: 142 },
    { user_id: USER_MEMBER1_ID, email: "bob@acmecorp.com", total_bytes: 4_294_967_296, file_count: 87 },
    { user_id: USER_MEMBER2_ID, email: "carol@acmecorp.com", total_bytes: 3_221_225_472, file_count: 63 },
    { user_id: USER_MEMBER3_ID, email: "dave@acmecorp.com", total_bytes: 1_770_000_000, file_count: 31 },
  ],
};

// ---- Billing usage ----
export const BILLING_USAGE = {
  tier: "business",
  storage_used_bytes: 15_728_640_000,
  storage_limit_bytes: 1_099_511_627_776, // 1 TB
  bandwidth_used_bytes_month: 52_428_800_000, // ~48.8 GB
  bandwidth_limit_bytes_month: 1_099_511_627_776,
  user_count: 4,
  user_limit: 250,
  plan_configured: true,
};

// ---- Client room templates ----
export const CLIENT_ROOM_TEMPLATES = {
  templates: [
    { name: "agency", sub_folders: ["Briefs", "Deliverables", "Feedback", "Assets"] },
    { name: "accounting", sub_folders: ["Tax Returns", "Receipts", "Financial Statements", "Correspondence"] },
    { name: "legal", sub_folders: ["Contracts", "Discovery", "Filings", "Correspondence"] },
    { name: "construction", sub_folders: ["Blueprints", "Permits", "Invoices", "Photos"] },
    { name: "clinic", sub_folders: ["Patient Records", "Lab Results", "Prescriptions", "Insurance"] },
  ],
};

// ---- Search results ----
export const SEARCH_RESULTS = {
  query: "architecture",
  limit: 20,
  offset: 0,
  hits: [
    { type: "file", id: FILE1_ID, name: "architecture-overview.pdf", path: "/Engineering/architecture-overview.pdf", workspace_id: WS_ID, folder_id: FOLDER_ENGINEERING_ID, updated_at: yesterday },
    { type: "folder", id: FOLDER_ENGINEERING_ID, name: "Engineering", path: "/Engineering/", workspace_id: WS_ID, folder_id: null, updated_at: yesterday },
  ],
};

// ---- Share links (for ShareDialog) ----
export const SHARE_LINK_RESPONSE = {
  id: "sl-001",
  workspace_id: WS_ID,
  resource_type: "folder",
  resource_id: FOLDER_ENGINEERING_ID,
  token: "demo-share-token-abc123",
  role: "viewer",
  password_protected: false,
  expires_at: null,
  max_downloads: null,
  download_count: 0,
  created_by: USER_ADMIN_ID,
  created_at: now,
  revoked_at: null,
};

// ---- Placement policy ----
export const PLACEMENT_POLICY = {
  tenant: "acmecorp",
  bucket: "acmecorp-zk-drive",
  policy: {
    encryption: { mode: "SSE-KMS", kms: "arn:aws:kms:us-east-1:123456789:key/demo-key" },
    placement: { provider: ["aws"], region: ["us-east-1", "eu-west-1"], country: ["US", "DE"], storage_class: ["STANDARD"], cache_location: "us-east-1" },
  },
};

// ---- CMK ----
export const CMK_RESPONSE = { cmk_uri: "arn:aws:kms:us-east-1:123456789:key/mrk-demo-cmk-key-id" };

// ---- KChat rooms ----
export const KCHAT_ROOMS = {
  rooms: [
    { id: "kr-001", workspace_id: WS_ID, kchat_room_id: "general", folder_id: FOLDER_ENGINEERING_ID, created_by: USER_ADMIN_ID, created_at: weekAgo },
    { id: "kr-002", workspace_id: WS_ID, kchat_room_id: "legal-team", folder_id: FOLDER_LEGAL_ID, created_by: USER_ADMIN_ID, created_at: twoDaysAgo },
  ],
};

/**
 * installDemoMocks intercepts all API routes with realistic mock data.
 * Call this at the start of each demo screenshot test, BEFORE any navigation.
 * This uses the same page.route() pattern as file-upload.spec.ts.
 */
export async function installDemoMocks(page: Page) {
  // Auth: signup and login both return a valid-looking token payload
  const authResponse = {
    token: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.demo",
    user_id: USER_ADMIN_ID,
    workspace_id: WS_ID,
    role: "admin",
  };

  await page.route("**/api/auth/signup", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(authResponse) });
  });

  await page.route("**/api/auth/login", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(authResponse) });
  });

  // Folders: matches both list (GET /api/folders[?...]) and detail
  // (GET /api/folders/{id}). Regex used instead of glob because
  // Playwright's `?` glob matcher is ambiguous between the literal
  // `?` of a query string and a single-char wildcard, which would
  // otherwise let the list pattern accidentally swallow detail URLs.
  await page.route(/\/api\/folders(\?|\/[^/]+|$)/, async (route) => {
    const method = route.request().method();
    const url = new URL(route.request().url());
    const detailMatch = url.pathname.match(/\/api\/folders\/([^/?]+)$/);

    // POST /api/folders -> create folder
    if (method === "POST" && !detailMatch) {
      const body = route.request().postDataJSON();
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          id: "f0000099-0000-0000-0000-000000000099",
          workspace_id: WS_ID,
          parent_folder_id: body?.parent_folder_id ?? null,
          name: body?.name ?? "New Folder",
          path: `/${body?.name ?? "New Folder"}/`,
          encryption_mode: body?.encryption_mode ?? "managed_encrypted",
          created_at: now,
          updated_at: now,
        }),
      });
      return;
    }

    // GET /api/folders?parent_folder_id=... -> root or subfolder list
    if (method === "GET" && !detailMatch) {
      const parentId = url.searchParams.get("parent_folder_id");
      if (!parentId || parentId === "root") {
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ folders: ROOT_FOLDERS }) });
      } else {
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ folders: [] }) });
      }
      return;
    }

    // GET /api/folders/{id} -> folder + children + files
    if (method === "GET" && detailMatch) {
      const id = detailMatch[1];
      if (id === FOLDER_ENGINEERING_ID) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            folder: ROOT_FOLDERS[0],
            children: ENGINEERING_CHILDREN,
            files: ENGINEERING_FILES,
          }),
        });
        return;
      }
      const folder = ROOT_FOLDERS.find((f) => f.id === id) ?? { ...ROOT_FOLDERS[0], id, name: "Folder" };
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ folder, children: [], files: [] }),
      });
      return;
    }

    await route.fallback();
  });

  // Admin: users
  await page.route("**/api/admin/users", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ADMIN_USERS) });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ADMIN_USERS.users[0]) });
    }
  });

  // Admin: audit log
  await page.route("**/api/admin/audit-log*", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(AUDIT_LOG) });
  });

  // Admin: retention policies
  await page.route("**/api/admin/retention-policies*", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(RETENTION_POLICIES) });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(RETENTION_POLICIES.policies[0]) });
    }
  });

  // Admin: storage usage
  await page.route("**/api/admin/storage-usage", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(STORAGE_USAGE) });
  });

  // Billing usage
  await page.route("**/api/admin/billing/usage", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(BILLING_USAGE) });
  });

  // Billing checkout/portal (return fake URLs)
  await page.route("**/api/admin/billing/checkout-session", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ url: "https://checkout.stripe.com/demo" }) });
  });
  await page.route("**/api/admin/billing/portal-session", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ url: "https://billing.stripe.com/demo" }) });
  });

  // Client room templates
  await page.route("**/api/client-rooms/templates", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(CLIENT_ROOM_TEMPLATES) });
  });

  // Search
  await page.route("**/api/search*", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SEARCH_RESULTS) });
  });

  // Share links
  await page.route("**/api/share-links", async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify(SHARE_LINK_RESPONSE) });
    } else {
      await route.fallback();
    }
  });

  // Placement
  await page.route("**/api/admin/placement", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(PLACEMENT_POLICY) });
  });

  // CMK
  await page.route("**/api/admin/cmk", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(CMK_RESPONSE) });
  });

  // KChat rooms
  await page.route("**/api/kchat/rooms", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(KCHAT_ROOMS) });
    } else {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify(KCHAT_ROOMS.rooms[0]) });
    }
  });

  // File preview URLs: the FilePreview component fires one of these per
  // visible file on render. Returning 404 makes the component fall back
  // to its mime-type-aware placeholder, exactly as it would on a fresh
  // upload that the preview worker hasn't processed yet. A 401 here
  // would trigger the global response interceptor and redirect to
  // /login, breaking every screenshot that lists files.
  await page.route(/\/api\/files\/[^/]+\/preview-url/, async (route) => {
    await route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify({ error: "preview not available" }) });
  });

  // File download URLs: harmless fake so any "Download" click doesn't
  // surface a 401.
  await page.route(/\/api\/files\/[^/]+\/download-url/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ download_url: "https://example.com/demo-download", object_key: "demo/object" }),
    });
  });

  // File tags listing (rendered in some panels). Empty list keeps the UI clean.
  await page.route(/\/api\/files\/[^/]+\/tags/, async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ tags: [] }) });
    } else {
      await route.fallback();
    }
  });

  // Notifications (empty but valid)
  await page.route("**/api/notifications*", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ notifications: [] }) });
  });

  // Activity
  await page.route("**/api/activity*", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ entries: [] }) });
  });

  // Inject auth tokens into localStorage so RequireAuth doesn't redirect to /login
  await page.addInitScript((auth) => {
    localStorage.setItem("zkdrive.token", auth.token);
    localStorage.setItem("zkdrive.workspace_id", auth.workspace_id);
    localStorage.setItem("zkdrive.role", auth.role);
  }, authResponse);
}
