import { expect, test } from "@playwright/test";
import * as fs from "fs";
import * as path from "path";
import { fileURLToPath } from "url";
import { FOLDER_ENGINEERING_ID, installDemoMocks } from "./demo-fixtures";

// Demo screenshots for documentation. Every test installs the full mock
// layer first, then navigates. No live backend is required — all API
// calls return realistic fake data via page.route(), the same pattern
// used by file-upload.spec.ts.
//
// Each screen is captured twice: once in light mode and once in dark
// mode. Light keeps the canonical, unsuffixed filename (e.g.
// 01-login-page.png) so existing docs keep resolving; the dark variant
// is additive (01-login-page-dark.png). The theme is pinned before the
// SPA boots via installDemoMocks({ theme }).

const here = path.dirname(fileURLToPath(import.meta.url));
const screenshotDir = path.resolve(here, "../../docs/screenshots");

const THEMES = ["light", "dark"] as const;
type Theme = (typeof THEMES)[number];

// shotPath returns the on-disk path for a screenshot, suffixing the dark
// variant only so light stays at the canonical filename.
function shotPath(name: string, theme: Theme): string {
  const suffix = theme === "dark" ? "-dark" : "";
  return `${screenshotDir}/${name}${suffix}.png`;
}

test.beforeAll(() => {
  fs.mkdirSync(screenshotDir, { recursive: true });
});

for (const theme of THEMES) {
  test.describe(`Demo Flow Screenshots (${theme})`, () => {
    test("01 - Login page", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/login");
      await page.waitForLoadState("networkidle");
      await page.screenshot({ path: shotPath("01-login-page", theme), fullPage: true });
    });

    test("02 - Signup page", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/signup");
      await page.waitForLoadState("networkidle");
      await page.screenshot({ path: shotPath("02-signup-page", theme), fullPage: true });
    });

    test("03 - Drive root with populated folders", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      // Should show 5 folders with encryption badges (managed + strict-ZK)
      await expect(page.getByText("Engineering").first()).toBeVisible();
      await expect(page.getByText("Legal Contracts").first()).toBeVisible();
      await page.screenshot({ path: shotPath("03-drive-root-folders", theme), fullPage: true });
    });

    test("04 - Inside folder with subfolders and files", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto(`/drive/folder/${FOLDER_ENGINEERING_ID}`);
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      // Should show Architecture/Releases subfolders + 5 files
      await expect(page.getByText("architecture-overview.pdf")).toBeVisible();
      await expect(page.getByText("Architecture").first()).toBeVisible();
      await page.screenshot({ path: shotPath("04-folder-contents", theme), fullPage: true });
    });

    test("05 - Create folder dialog (privacy mode picker)", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await page.getByRole("button", { name: /^new folder$/i }).click();
      // Dialog now uses the customer-facing mode names from PROPOSAL §3.3:
      // "Confidential managed (default)" (server-readable) vs
      // "Strict zero-knowledge" (server-blind). See PrivacyPage.tsx.
      await expect(page.getByText(/confidential managed/i).first()).toBeVisible();
      await expect(page.getByText(/strict zero-knowledge/i).first()).toBeVisible();
      await page.screenshot({ path: shotPath("05-create-folder-dialog", theme), fullPage: true });
    });

    test("06 - Create folder dialog with strict-ZK warning", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await page.getByRole("button", { name: /^new folder$/i }).click();
      // Switch to strict-ZK and capture the honest disclosure callout.
      // The radio label now reads "Strict zero-knowledge - end-to-end
      // encrypted ... Previews, full-text search, and virus scanning are
      // disabled" — `getByLabel` matches against the full label text,
      // which now also matches the per-mode body copy. Use the input
      // selector directly so we stay robust to body-copy edits.
      await page.locator('input[name="encmode"][value="strict_zk"]').check();
      await expect(page.getByRole("alert")).toContainText(/irreversible/i);
      await page.screenshot({ path: shotPath("06-create-folder-strict-zk", theme), fullPage: true });
    });

    test("07 - Search results dropdown", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await page.getByPlaceholder(/search/i).fill("architecture");
      // Debounce in SearchBar is 250ms
      await page.waitForTimeout(500);
      await expect(page.getByText("architecture-overview.pdf").first()).toBeVisible();
      await page.screenshot({ path: shotPath("07-search-results", theme), fullPage: true });
    });

    test("08 - Create-from-template dialog", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/drive");
      await page.waitForLoadState("networkidle");
      await page.getByRole("button", { name: /create from template/i }).click();
      await page.waitForTimeout(300);
      await page.screenshot({ path: shotPath("08-template-dialog", theme), fullPage: true });
    });

    test("09 - Admin: Users tab", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await expect(page.getByText("alice@northwind.example")).toBeVisible();
      await expect(page.getByText("Bob Martinez")).toBeVisible();
      await page.screenshot({ path: shotPath("09-admin-users", theme), fullPage: true });
    });

    test("10 - Admin: Audit log tab", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin");
      await page.waitForLoadState("networkidle");
      await page.getByRole("tab", { name: /^audit$/i }).click();
      await page.waitForTimeout(500);
      await expect(page.getByText(/file\.upload|folder\.create/).first()).toBeVisible();
      await page.screenshot({ path: shotPath("10-admin-audit-log", theme), fullPage: true });
    });

    test("11 - Admin: Retention policies tab", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin");
      await page.waitForLoadState("networkidle");
      await page.getByRole("tab", { name: /^retention$/i }).click();
      await page.waitForTimeout(500);
      await page.screenshot({ path: shotPath("11-admin-retention", theme), fullPage: true });
    });

    test("12 - Admin: Storage usage tab", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin");
      await page.waitForLoadState("networkidle");
      await page.getByRole("tab", { name: /^storage$/i }).click();
      await page.waitForTimeout(500);
      await expect(page.getByText("alice@northwind.example")).toBeVisible();
      await page.screenshot({ path: shotPath("12-admin-storage", theme), fullPage: true });
    });

    test("13 - Billing page (tier + usage)", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/billing");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await expect(page.getByRole("heading", { name: "Your plan", exact: true })).toBeVisible();
      await expect(page.getByRole("heading", { name: "Usage this period", exact: true })).toBeVisible();
      await page.screenshot({ path: shotPath("13-billing", theme), fullPage: true });
    });

    test("14 - Placement policy editor", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin/placement");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await expect(page.getByRole("heading", { name: /placement policy/i })).toBeVisible();
      await page.screenshot({ path: shotPath("14-placement-policy", theme), fullPage: true });
    });

    test("15 - Encryption (CMK) page", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin/encryption");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await expect(page.getByRole("heading", { name: /encryption \(cmk\)/i })).toBeVisible();
      await page.screenshot({ path: shotPath("15-encryption-cmk", theme), fullPage: true });
    });

    test("16 - KChat rooms admin", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin/kchat");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await expect(page.getByRole("heading", { name: /kchat rooms/i })).toBeVisible();
      await expect(page.getByText("general")).toBeVisible();
      await page.screenshot({ path: shotPath("16-kchat-rooms", theme), fullPage: true });
    });

    test("17 - Admin: Health dashboard tab", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/admin");
      await page.waitForLoadState("networkidle");
      await page.getByRole("tab", { name: /^health$/i }).click();
      await page.waitForTimeout(500);
      await page.screenshot({ path: shotPath("17-admin-health", theme), fullPage: true });
    });

    test("18 - Guided setup wizard", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/setup");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await page.screenshot({ path: shotPath("18-setup-wizard", theme), fullPage: true });
    });

    test("19 - Privacy explainer page", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/drive/privacy");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await page.screenshot({ path: shotPath("19-privacy", theme), fullPage: true });
    });

    test("20 - Collaborative documents list", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto(`/drive/folder/${FOLDER_ENGINEERING_ID}/documents`);
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      await expect(page.getByText("Q2 Planning Notes")).toBeVisible();
      await page.screenshot({ path: shotPath("20-documents-list", theme), fullPage: true });
    });

    test("21 - Two-factor (TOTP) enrollment", async ({ page }) => {
      await installDemoMocks(page, { theme });
      await page.goto("/account/2fa");
      await page.waitForLoadState("networkidle");
      await page.waitForTimeout(500);
      // Assert the enrollment step rendered (QR secret visible) so a
      // regression that bounces to /login fails loudly here.
      await expect(page.getByText("JBSWY3DPEHPK3PXP")).toBeVisible();
      await page.screenshot({ path: shotPath("21-two-factor-enroll", theme), fullPage: true });
    });
  });
}
