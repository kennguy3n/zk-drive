import { test, expect, type Page, type APIRequestContext } from "@playwright/test";
import * as fs from "fs";
import * as path from "path";
import { fileURLToPath } from "url";

// Blog evidence capture — runs against a LIVE zk-drive backend with real
// seeded data (see scripts/seed/seed.py). Unlike demo-screenshots.spec.ts
// this installs NO mocks: every screenshot reflects genuine API responses,
// real uploaded bytes in object storage, and hash-chained audit entries.
//
// Every screen is captured twice — light and dark. Light keeps the
// canonical, unsuffixed filename (e.g. 10-drive-root.png); the dark
// variant is additive (10-drive-root-dark.png). The colour scheme is
// pinned in localStorage before each navigation via pinTheme().

const here = path.dirname(fileURLToPath(import.meta.url));
const shotDir = path.resolve(here, "../../docs/blog/img");

// The seed writes its state file next to the script. Allow an override
// (BLOG_SEED_STATE) for bespoke capture environments, but default to the
// in-repo location so a developer who just ran scripts/seed/seed.py gets
// evidence capture for free.
const statePath =
  process.env.BLOG_SEED_STATE ?? path.resolve(here, "../../scripts/seed/out/state.json");
const hasSeed = fs.existsSync(statePath);
const seed = hasSeed ? JSON.parse(fs.readFileSync(statePath, "utf-8")) : { folders: {} };
const F = (seed.folders ?? {}) as Record<string, string>;

const THEMES = ["light", "dark"] as const;
type Theme = (typeof THEMES)[number];

// Conditionally skip the entire suite when no live seed is present. The output
// dir is created in a describe-scoped beforeAll (see the first describe below),
// so nothing runs when the suite is skipped.
const describe = hasSeed ? test.describe : test.describe.skip;

// pinTheme registers an init script so localStorage.zkdrive.theme is set
// before the SPA boots on every subsequent navigation in this page. We
// pin an explicit "light"/"dark" (never "system") so captures never
// depend on the runner's prefers-color-scheme.
async function pinTheme(page: Page, theme: Theme) {
  await page.addInitScript((t) => {
    localStorage.setItem("zkdrive.theme", t);
  }, theme);
}

async function apiLogin(request: APIRequestContext, email: string, password = "DemoPass!2026") {
  const r = await request.post("/api/auth/login", { data: { email, password } });
  if (!r.ok()) throw new Error(`login ${email} -> ${r.status()}`);
  const grant = await r.json();
  // The controlled seed enrolls no MFA, so login returns the grant directly. If
  // a future seed enables MFA the response is { mfa_required, mfa_token, ... };
  // fail loudly here instead of silently storing "undefined" tokens downstream.
  if (!grant?.token) {
    throw new Error(`login ${email}: unexpected response (mfa_required=${grant?.mfa_required ?? false})`);
  }
  return grant;
}

// Seed localStorage exactly like the SPA's client.ts does after a real login;
// the per-test page.goto(...) then boots the SPA authenticated against the
// live backend (this helper itself does not reload). Returns the grant so
// callers can reuse the token for direct API calls.
async function authAs(page: Page, request: APIRequestContext, email: string) {
  const id = await apiLogin(request, email);
  await page.goto("/login");
  await page.evaluate((s) => {
    localStorage.setItem("zkdrive.token", s.token);
    localStorage.setItem("zkdrive.workspace_id", s.workspace_id);
    localStorage.setItem("zkdrive.role", s.role);
    localStorage.setItem("zkdrive.user_id", s.user_id);
  }, id);
  return id;
}

function shot(page: Page, name: string, theme: Theme) {
  const suffix = theme === "dark" ? "-dark" : "";
  return (async () => {
    await page.waitForTimeout(450);
    await page.screenshot({ path: `${shotDir}/${name}${suffix}.png`, fullPage: true });
  })();
}

test.beforeAll(() => {
  if (hasSeed) fs.mkdirSync(shotDir, { recursive: true });
});

for (const theme of THEMES) {
  // ---------- Unauthenticated marketing-facing entry points ----------
  describe(`public (${theme})`, () => {
    test.beforeEach(async ({ page }) => pinTheme(page, theme));

    test("login + signup pages", async ({ page }) => {
      await page.goto("/login");
      await page.waitForLoadState("networkidle");
      await shot(page, "00-login", theme);
      await page.goto("/signup");
      await page.waitForLoadState("networkidle");
      await shot(page, "01-signup", theme);
    });
  });

  // ---------- Admin / workspace owner (Alice) ----------
  describe(`admin owner journeys (${theme})`, () => {
    test.beforeEach(async ({ page, request }) => {
      await pinTheme(page, theme);
      await authAs(page, request, "alice@northwind.example");
    });

    test("drive root with privacy badges", async ({ page }) => {
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await expect(page.getByText("Engineering").first()).toBeVisible();
      await shot(page, "10-drive-root", theme);
    });

    test("folder contents with real files", async ({ page }) => {
      await page.goto(`/drive/folder/${F.engineering}`);
      await page.waitForLoadState("networkidle");
      await expect(page.getByText("architecture-overview.pdf").first()).toBeVisible();
      await shot(page, "11-folder-engineering", theme);
    });

    test("create-folder privacy picker + strict-ZK disclosure", async ({ page }) => {
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await page.getByRole("button", { name: /^new folder$/i }).click();
      await page.waitForTimeout(300);
      await shot(page, "12-create-folder-dialog", theme);
      const zk = page.locator('input[name="encmode"][value="strict_zk"]');
      if (await zk.count()) {
        await zk.check();
        await page.waitForTimeout(300);
        await shot(page, "13-create-folder-strict-zk", theme);
      }
    });

    test("privacy explainer page", async ({ page }) => {
      await page.goto("/drive/privacy");
      await page.waitForLoadState("networkidle");
      await shot(page, "14-privacy-page", theme);
    });

    test("search across real content", async ({ page }) => {
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      const box = page.getByPlaceholder(/search/i).first();
      await box.fill("architecture");
      await page.waitForTimeout(700);
      await shot(page, "15-search-results", theme);
    });

    test("share dialog (link with password/expiry)", async ({ page }) => {
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      const share = page.getByRole("button", { name: /^share$/i }).first();
      if (await share.count()) {
        await share.click();
        await page.waitForTimeout(500);
        await shot(page, "30-share-dialog", theme);
      }
    });

    test("admin tabs: users / audit / retention / storage", async ({ page }) => {
      await page.goto("/admin");
      await page.waitForLoadState("networkidle");
      await expect(page.getByText("alice@northwind.example").first()).toBeVisible();
      await shot(page, "20-admin-users", theme);
      for (const [tab, name] of [
        [/^audit$/i, "21-admin-audit"],
        [/^retention$/i, "22-admin-retention"],
        [/^storage$/i, "23-admin-storage"],
      ] as const) {
        const btn = page.getByRole("tab", { name: tab }).first();
        if (await btn.count()) {
          await btn.click();
          await page.waitForTimeout(500);
          await shot(page, name, theme);
        }
      }
    });

    test("billing / placement / encryption / kchat / setup", async ({ page }) => {
      for (const [route, name] of [
        ["/billing", "25-billing"],
        ["/admin/placement", "26-admin-placement"],
        ["/admin/encryption", "27-admin-encryption"],
        ["/admin/kchat", "28-admin-kchat"],
        ["/setup", "40-setup-wizard"],
      ] as const) {
        await page.goto(route);
        await page.waitForLoadState("networkidle");
        await shot(page, name, theme);
      }
    });
  });

  // ---------- Collaborative documents (live presence) ----------
  describe(`collaborative documents (${theme})`, () => {
    test("documents list + live editor", async ({ page, request }) => {
      await pinTheme(page, theme);
      const grant = await authAs(page, request, "alice@northwind.example");

      await page.goto(`/drive/folder/${F.engineering}/documents`);
      await page.waitForLoadState("networkidle");
      await expect(page.getByText("Q2 Planning Notes").first()).toBeVisible();
      await shot(page, "60-documents-list", theme);

      // Resolve the real document id from the live API, then open the
      // TipTap + Yjs editor so the screenshot shows the genuine
      // collaborative surface (toolbar, presence, connection chip).
      const res = await request.get(`/api/folders/${F.engineering}/documents`, {
        headers: { Authorization: `Bearer ${grant.token}` },
      });
      const body = (await res.json()) as { documents?: Array<{ id: string }> };
      const docId = body.documents?.[0]?.id;
      if (docId) {
        await page.goto(`/drive/document/${docId}`);
        await page.waitForLoadState("networkidle");
        // Allow the WebSocket to connect and presence to settle.
        await page.waitForTimeout(1500);
        await shot(page, "61-document-editor", theme);
      }
    });
  });

  // ---------- Account security (real TOTP enrollment) ----------
  describe(`account security (${theme})`, () => {
    test("two-factor (TOTP) enrollment", async ({ page, request }) => {
      await pinTheme(page, theme);
      await authAs(page, request, "carol@northwind.example");
      await page.goto("/account/2fa");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(700);
      // The seed enrolls no MFA, so this renders the live enrollment
      // step with a real, scannable QR generated by the backend.
      await shot(page, "41-two-factor-enroll", theme);
    });
  });

  // ---------- Compliance / security officer + NoOps gaps ----------
  describe(`compliance + ops (${theme})`, () => {
    test.beforeEach(async ({ page, request }) => {
      await pinTheme(page, theme);
      await authAs(page, request, "alice@northwind.example");
    });

    test("admin health (observability / NoOps)", async ({ page }) => {
      await page.goto("/admin");
      await page.waitForLoadState("networkidle");
      const health = page.getByRole("tab", { name: /^health$/i }).first();
      if (await health.count()) {
        await health.click();
        await page.waitForTimeout(700);
        await shot(page, "24-admin-health", theme);
      }
    });

    test("strict zero-knowledge folder (no preview/search)", async ({ page }) => {
      await page.goto(`/drive/folder/${F.legal}`);
      await page.waitForLoadState("networkidle");
      await shot(page, "16-strict-zk-folder", theme);
    });

    test("guest invite by email", async ({ page }) => {
      // Guest invites are folder-scoped, so open a folder share from the
      // drive root (always present) and switch to the invite tab. This is
      // deterministic — it never depends on a folder containing a file.
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await page.getByRole("button", { name: /^share$/i }).first().click();
      await page.waitForTimeout(400);
      await page.getByRole("tab", { name: /invite by email/i }).click();
      await page.waitForTimeout(400);
      await expect(page.getByPlaceholder(/guest@/i)).toBeVisible();
      await shot(page, "31-guest-invite", theme);
    });
  });

  // ---------- Knowledge worker (Bob, member) ----------
  describe(`knowledge worker (${theme})`, () => {
    test("bob drive + shared engineering folder", async ({ page, request }) => {
      await pinTheme(page, theme);
      await authAs(page, request, "bob@northwind.example");
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await shot(page, "50-bob-drive", theme);
      await page.goto(`/drive/folder/${F.engineering}`);
      await page.waitForLoadState("networkidle");
      await shot(page, "51-bob-folder-engineering", theme);
    });
  });
}
