import { test, expect, type Page, type APIRequestContext } from "@playwright/test";
import * as fs from "fs";
import * as path from "path";
import { fileURLToPath } from "url";

// Blog evidence capture — runs against a LIVE zk-drive backend with real
// seeded data (see /home/ubuntu/seed/seed.py). Unlike demo-screenshots.spec.ts
// this installs NO mocks: every screenshot reflects genuine API responses,
// real uploaded bytes in object storage, and hash-chained audit entries.

const here = path.dirname(fileURLToPath(import.meta.url));
const shotDir = path.resolve(here, "../../docs/blog/img");

// This suite captures the published blog evidence and ONLY runs against a live,
// seeded backend (see scripts referenced in docs/blog/README.md). It is not part
// of the mocked demo-screenshot flow. When the seed state file is absent — i.e.
// anywhere other than the evidence-capture environment — the whole suite skips
// cleanly so it never breaks the standard `npx playwright test` run.
const statePath = process.env.BLOG_SEED_STATE ?? "/home/ubuntu/seed/out/state.json";
const hasSeed = fs.existsSync(statePath);
const seed = hasSeed ? JSON.parse(fs.readFileSync(statePath, "utf-8")) : { folders: {} };
const F = seed.folders as Record<string, string>;

// Conditionally skip the entire suite when no live seed is present. The output
// dir is created in a describe-scoped beforeAll (see the first describe below),
// so nothing runs when the suite is skipped.
const describe = hasSeed ? test.describe : test.describe.skip;

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
// live backend (this helper itself does not reload).
async function authAs(page: Page, request: APIRequestContext, email: string) {
  const id = await apiLogin(request, email);
  await page.goto("/login");
  await page.evaluate((s) => {
    localStorage.setItem("zkdrive.token", s.token);
    localStorage.setItem("zkdrive.workspace_id", s.workspace_id);
    localStorage.setItem("zkdrive.role", s.role);
    localStorage.setItem("zkdrive.user_id", s.user_id);
  }, id);
}

async function shot(page: Page, name: string) {
  await page.waitForTimeout(450);
  await page.screenshot({ path: `${shotDir}/${name}.png`, fullPage: true });
}

// ---------- Unauthenticated marketing-facing entry points ----------
describe("public", () => {
  // First describe in the suite; only runs when seeded, so this is where the
  // screenshot output dir is ensured (matches demo-screenshots.spec.ts).
  test.beforeAll(() => fs.mkdirSync(shotDir, { recursive: true }));

  test("login + signup pages", async ({ page }) => {
    await page.goto("/login");
    await page.waitForLoadState("networkidle");
    await shot(page, "00-login");
    await page.goto("/signup");
    await page.waitForLoadState("networkidle");
    await shot(page, "01-signup");
  });
});

// ---------- Admin / workspace owner (Alice) ----------
describe("admin owner journeys", () => {
  test.beforeEach(async ({ page, request }) => {
    await authAs(page, request, "alice@northwind.example");
  });

  test("drive root with privacy badges", async ({ page }) => {
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await expect(page.getByText("Engineering").first()).toBeVisible();
    await shot(page, "10-drive-root");
  });

  test("folder contents with real files", async ({ page }) => {
    await page.goto(`/drive/folder/${F.engineering}`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByText("architecture-overview.pdf").first()).toBeVisible();
    await shot(page, "11-folder-engineering");
  });

  test("create-folder privacy picker + strict-ZK disclosure", async ({ page }) => {
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: /^new folder$/i }).click();
    await page.waitForTimeout(300);
    await shot(page, "12-create-folder-dialog");
    const zk = page.locator('input[name="encmode"][value="strict_zk"]');
    if (await zk.count()) {
      await zk.check();
      await page.waitForTimeout(300);
      await shot(page, "13-create-folder-strict-zk");
    }
  });

  test("privacy explainer page", async ({ page }) => {
    await page.goto("/drive/privacy");
    await page.waitForLoadState("networkidle");
    await shot(page, "14-privacy-page");
  });

  test("search across real content", async ({ page }) => {
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    const box = page.getByPlaceholder(/search/i).first();
    await box.fill("architecture");
    await page.waitForTimeout(700);
    await shot(page, "15-search-results");
  });

  test("share dialog (link with password/expiry)", async ({ page }) => {
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    const share = page.getByRole("button", { name: /^share$/i }).first();
    if (await share.count()) {
      await share.click();
      await page.waitForTimeout(500);
      await shot(page, "30-share-dialog");
    }
  });

  test("admin tabs: users / audit / retention / storage", async ({ page }) => {
    await page.goto("/admin");
    await page.waitForLoadState("networkidle");
    await expect(page.getByText("alice@northwind.example").first()).toBeVisible();
    await shot(page, "20-admin-users");
    for (const [tab, name] of [
      [/^audit$/i, "21-admin-audit"],
      [/^retention$/i, "22-admin-retention"],
      [/^storage$/i, "23-admin-storage"],
    ] as const) {
      const btn = page.getByRole("button", { name: tab }).first();
      if (await btn.count()) {
        await btn.click();
        await page.waitForTimeout(500);
        await shot(page, name);
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
      await shot(page, name);
    }
  });
});

// ---------- Compliance / security officer + NoOps gaps ----------
describe("compliance + ops", () => {
  test.beforeEach(async ({ page, request }) => {
    await authAs(page, request, "alice@northwind.example");
  });

  test("admin health (observability / NoOps)", async ({ page }) => {
    await page.goto("/admin");
    await page.waitForLoadState("networkidle");
    const health = page.getByRole("button", { name: /^health$/i }).first();
    if (await health.count()) {
      await health.click();
      await page.waitForTimeout(700);
      await shot(page, "24-admin-health");
    }
    const storage = page.getByRole("button", { name: /^storage$/i }).first();
    if (await storage.count()) {
      await storage.click();
      await page.waitForTimeout(500);
      await shot(page, "23-admin-storage");
    }
  });

  test("strict zero-knowledge folder (no preview/search)", async ({ page }) => {
    await page.goto(`/drive/folder/${F.legal}`);
    await page.waitForLoadState("networkidle");
    await shot(page, "16-strict-zk-folder");
  });

  test("guest invite by email", async ({ page }) => {
    await page.goto(`/drive/folder/${F.vault}`);
    await page.waitForLoadState("networkidle");
    const share = page.getByRole("button", { name: /^share$/i }).first();
    if (await share.count()) {
      await share.click();
      await page.waitForTimeout(400);
      const invite = page.getByRole("button", { name: /invite by email/i }).first();
      if (await invite.count()) {
        await invite.click();
        await page.waitForTimeout(400);
        await shot(page, "31-guest-invite");
      }
    }
  });
});

// ---------- Knowledge worker (Bob, member) ----------
describe("knowledge worker", () => {
  test("bob drive + shared engineering folder", async ({ page, request }) => {
    await authAs(page, request, "bob@northwind.example");
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await shot(page, "50-bob-drive");
    await page.goto(`/drive/folder/${F.engineering}`);
    await page.waitForLoadState("networkidle");
    await shot(page, "51-bob-folder-engineering");
  });
});
