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
const FOLDER_ARCHITECTURE_ID = "f0000006-0000-0000-0000-000000000006";
const FOLDER_RELEASES_ID = "f0000007-0000-0000-0000-000000000007";
export const DOCUMENT_ID = "d0000001-0000-0000-0000-000000000001";

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
  { id: FOLDER_ARCHITECTURE_ID, workspace_id: WS_ID, parent_folder_id: FOLDER_ENGINEERING_ID, name: "Architecture", path: "/Engineering/Architecture/", encryption_mode: "managed_encrypted", created_at: weekAgo, updated_at: yesterday },
  { id: FOLDER_RELEASES_ID, workspace_id: WS_ID, parent_folder_id: FOLDER_ENGINEERING_ID, name: "Releases", path: "/Engineering/Releases/", encryption_mode: "managed_encrypted", created_at: weekAgo, updated_at: twoDaysAgo },
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
    { id: USER_ADMIN_ID, email: "alice@northwind.example", name: "Alice Chen", role: "admin", workspace_id: WS_ID, deactivated_at: null, created_at: weekAgo },
    { id: USER_MEMBER1_ID, email: "bob@northwind.example", name: "Bob Martinez", role: "member", workspace_id: WS_ID, deactivated_at: null, created_at: weekAgo },
    { id: USER_MEMBER2_ID, email: "carol@northwind.example", name: "Carol Nguyen", role: "member", workspace_id: WS_ID, deactivated_at: null, created_at: twoDaysAgo },
    { id: USER_MEMBER3_ID, email: "dave@northwind.example", name: "Dave Patel", role: "admin", workspace_id: WS_ID, deactivated_at: null, created_at: yesterday },
    { id: "ff000001-0000-0000-0000-000000000001", email: "eve@northwind.example", name: "Eve Thompson", role: "member", workspace_id: WS_ID, deactivated_at: twoDaysAgo, created_at: weekAgo },
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
    { user_id: USER_ADMIN_ID, email: "alice@northwind.example", total_bytes: 6_442_450_944, file_count: 142 },
    { user_id: USER_MEMBER1_ID, email: "bob@northwind.example", total_bytes: 4_294_967_296, file_count: 87 },
    { user_id: USER_MEMBER2_ID, email: "carol@northwind.example", total_bytes: 3_221_225_472, file_count: 63 },
    { user_id: USER_MEMBER3_ID, email: "dave@northwind.example", total_bytes: 1_770_000_000, file_count: 31 },
  ],
};

// ---- Feature flags ----
// The SPA's FeaturesProvider fetches GET /api/features on mount for every
// authenticated page. The demo workspace is a fully-featured "business"
// tier with every capability enabled so the gated admin tabs (CMK, data
// residency, retention, KChat, …) render in the screenshots. Keys mirror
// frontend/src/features/featureKeys.ts.
export const FEATURES_RESPONSE = {
  tier: "business",
  features: {
    folders: true,
    files: true,
    share_links: true,
    basic_search: true,
    sso: true,
    audit_log: true,
    retention_policies: true,
    onlyoffice: true,
    client_rooms: true,
    webhooks: true,
    kchat: true,
    strict_zk: true,
    cmk: true,
    data_residency: true,
    ai_summaries: true,
  },
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
  tenant: "northwind",
  bucket: "northwind-zk-drive",
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

// ---- Admin: Health dashboard ----
// Shape mirrors HealthReport in client.ts. A fully-green deployment with
// every subsystem configured, so the admin Health tab renders the full
// traffic-light roll-up rather than greying out unconfigured rows.
export const HEALTH_REPORT = {
  status: "green",
  generated_at: now,
  subsystems: [
    { name: "postgres", status: "green", detail: { pool_in_use: 3, pool_idle: 9, max_conns: 20 } },
    { name: "object_storage", status: "green", detail: { provider: "s3", bucket: "northwind-zk-drive", reachable: true } },
    { name: "nats", status: "green", detail: { stream: "DRIVE_JOBS", pending: 0, consumers: 1 } },
    { name: "worker", status: "green", detail: { last_seen: now, jobs_processed: 1284 } },
    { name: "virus_scanner", status: "green", detail: { engine: "clamd", reachable: true } },
    { name: "collaboration", status: "green", detail: { sessions: 2 } },
  ],
};

// ---- Guided setup wizard status ----
// needs_setup=true keeps SetupWizardPage on the wizard instead of
// self-redirecting to /drive. Steps mirror internal/setup.Steps.
export const SETUP_STATUS = {
  setup_completed: false,
  needs_setup: true,
  steps: {
    admin_account: { configured: false },
    storage: { configured: true, detail: "S3 bucket northwind-zk-drive reachable" },
    workspace: { configured: false },
    optional_services: { email: true, virus_scanning: true, ai: false, collaborative_editing: true },
  },
};

// ---- Collaborative documents (TipTap + Yjs) ----
const DOC_COMMON = {
  workspace_id: WS_ID,
  folder_id: FOLDER_ENGINEERING_ID,
  encryption_mode: "managed_encrypted",
  capability: "full" as const,
  allowed_collab_modes: ["markdown", "rich", "rich_presence", "disabled"],
  y_state_seq_floor: 0,
  snapshot_version: 4,
  created_by: USER_ADMIN_ID,
};
export const DOCUMENTS = {
  documents: [
    { id: DOCUMENT_ID, name: "Q2 Planning Notes", collab_mode: "rich_presence", ...DOC_COMMON, created_at: weekAgo, updated_at: now },
    { id: "d0000002-0000-0000-0000-000000000002", name: "Architecture Decision Record", collab_mode: "rich", ...DOC_COMMON, created_at: twoDaysAgo, updated_at: yesterday },
    { id: "d0000003-0000-0000-0000-000000000003", name: "Release Checklist", collab_mode: "markdown", ...DOC_COMMON, created_at: yesterday, updated_at: now },
  ],
};
export const DOCUMENT_DETAIL = DOCUMENTS.documents[0];

// ---- TOTP / 2FA status (authenticated re-enrollment view) ----
export const TOTP_STATUS = {
  enabled: false,
  pending_enrollment: false,
  recovery_codes_remaining: 0,
};

// TOTP enrollment "begin" response. qr_code_png is a genuine QR (rendered
// from the otpauth URI below) so the enrollment screenshot shows a real,
// scannable code rather than a broken-image placeholder.
export const TOTP_ENROLL_BEGIN = {
  secret: "JBSWY3DPEHPK3PXP",
  otpauth_uri:
    "otpauth://totp/zk-drive:alice@northwind.example?secret=JBSWY3DPEHPK3PXP&issuer=zk-drive&algorithm=SHA1&digits=6&period=30",
  qr_code_png: "iVBORw0KGgoAAAANSUhEUgAAASYAAAEmAQAAAADnvwB3AAADM0lEQVR4nO2aTYokRwyFv2gl9FIJPoAvYlDdYI5k+mZR4Iv0AQzK5UAkzwtFlr1zGcZ0ZTJBUVRlaiEh6T39RBP/fva3J4Tgp9TXSaEEsGS4hssyhndTB5QBQIxX1f5pG0OSZUgd71IfoIwBw2WS8uw2LkAj6ts+v/0xYhH7r9nq/bban6+r/fM2AnjfF/YVq7/bDYLt9mV6/R82bqtyFfffGjRMNO/tC/X60TYKFte+xr5g+a1v694Yv9zrOdvrav+01L21xnZ7T94zgIWwjPfP35fttrfW1pfW/pmjebplDMIyJpuoW0a9OzmuTpqAAHBZAlhi0pgUeXbuQOqmXryvDMvyIPis8oZfwo8ujmIArxDtFbGSxun9+AZ3QBn7eme7VawC+8rwvq8sX6XXD5NCkjJMmrGqPlyDmBUscQXMUTdJ6sM1c9BliSXKwMUV8hG828Ed9bFEiRKT7AJ+TAZRXdUjGadbiTLz/DbG8OnE+ch7kePwPmuDV9X+ecwBjgonVOAjmaYfr8CPmjhTxzKA4dKBq1fwYzK8D7AE7xNqkuGSVGXe+W0sqIkix4KdyZj1+wKYIylDyYAijmITjh7k/Lj6BvdWBRvxnliyEAOkD2CBvd1eWfunpLabJWP+vQMD3jNau31fC29fWfunTg0bmYPW2Utahh195TXykcpE73bQIkc7eZF6VR1XzQGmva7ZfVym78jANSu6ibHTj/W5BD8CYep4H/xtb+Vmja1Ob6N6Fagcmx1LBsE08xIzq4mf4L0QBu9zjPMA2FfV/nlcHYU23vWIWELqs7e6RKweK8h44Iweo7naSL6q9v8FV/soxxHVVUGFbgeuYKN00AemuUeuhqvA9gqYoz6zDzgGVmOO5qLKntPbmP+0a14AqPlV4e355+TH3irB5xi59jjUdiDj/PXqG1trrS3MwWMN/00fUgeWmoG8rvZPneNeB4MwaRwz1bncyUvgakYlYEHo7CK9brBMgZPb+JC66/M2/GMh8A9g8d7WGMT5d+WHlHdc9nn7vjK2WzUdSizv18nHYsnqqqpldhXSXoIfocCTuYWcTqxNh1/gzkP7ebfzElJ/ATRM5y8tYyOCAAAAAElFTkSuQmCC",
};

/**
 * installDemoMocks intercepts all API routes with realistic mock data.
 * Call this at the start of each demo screenshot test, BEFORE any navigation.
 * This uses the same page.route() pattern as file-upload.spec.ts.
 */
export async function installDemoMocks(
  page: Page,
  opts: { theme?: "light" | "dark" } = {},
) {
  // Pin the colour scheme before the SPA boots so ThemeProvider applies
  // the .dark class (or not) on first paint. We set zkdrive.theme to an
  // explicit "light"/"dark" rather than "system" so screenshots never
  // depend on the CI runner's prefers-color-scheme.
  if (opts.theme) {
    await page.addInitScript((theme) => {
      localStorage.setItem("zkdrive.theme", theme);
    }, opts.theme);
  }

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

  // Feature flags: FeaturesProvider fetches this on mount for every
  // authenticated page. An unmocked 401 here trips the global response
  // interceptor and redirects to /login, blanking every authenticated
  // screenshot — so it must return a valid feature map.
  await page.route("**/api/features", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(FEATURES_RESPONSE) });
  });

  // OnlyOffice status: FileBrowserPage probes this on mount to decide
  // whether to show the office-editing affordance. Like /api/features,
  // an unmocked 401 here redirects to /login before the page's own
  // catch can swallow it.
  await page.route("**/api/onlyoffice/status", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ enabled: true }) });
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

  // Workspace default encryption mode (GET current + PUT to change). The
  // EncryptionPage's DefaultEncryptionModeSection loads this on mount; an
  // unmocked 401 would bounce /admin/encryption to /login.
  await page.route("**/api/admin/workspace/default-encryption-mode", async (route) => {
    const supported = ["managed_encrypted", "strict_zk"];
    if (route.request().method() === "PUT") {
      const body = route.request().postDataJSON();
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ mode: body?.mode ?? "managed_encrypted", supported }) });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ mode: "managed_encrypted", supported }) });
    }
  });

  // KChat rooms
  await page.route("**/api/kchat/rooms", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(KCHAT_ROOMS) });
    } else {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify(KCHAT_ROOMS.rooms[0]) });
    }
  });

  // Admin: health dashboard (traffic-light roll-up on the Health tab)
  await page.route("**/api/admin/health-dashboard", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(HEALTH_REPORT) });
  });

  // Guided setup wizard status (public; keeps the wizard on-screen)
  await page.route("**/api/setup/status", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SETUP_STATUS) });
  });

  // TOTP / 2FA status for the authenticated re-enrollment view
  await page.route("**/api/auth/totp/status", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TOTP_STATUS) });
  });

  // TOTP enrollment begin — returns the secret + a real QR PNG so the
  // /account/2fa screenshot shows the scannable enrollment step. Without
  // this the page's totpEnrollBegin() call would hit the live backend,
  // 401, and bounce to /login.
  await page.route("**/api/auth/totp/enroll/begin", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TOTP_ENROLL_BEGIN) });
  });

  // Collaborative documents: list under a folder. Registered after the
  // /api/folders route so it takes precedence for the /documents suffix
  // (Playwright matches the most-recently-registered route first).
  await page.route(/\/api\/folders\/[^/]+\/documents$/, async (route) => {
    if (route.request().method() === "POST") {
      const body = route.request().postDataJSON();
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ ...DOCUMENT_DETAIL, id: "d0000099-0000-0000-0000-000000000099", name: body?.name ?? "Untitled", collab_mode: body?.collab_mode ?? "rich_presence" }) });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(DOCUMENTS) });
    }
  });

  // Single document detail (GET /api/documents/{id})
  await page.route(/\/api\/documents\/[^/?]+$/, async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(DOCUMENT_DETAIL) });
    } else {
      await route.fallback();
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
