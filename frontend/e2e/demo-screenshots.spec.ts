import { expect, test } from "@playwright/test";
import * as fs from "fs";
import * as path from "path";
import { fileURLToPath } from "url";
import { FOLDER_ENGINEERING_ID, installDemoMocks } from "./demo-fixtures";

// Demo screenshots for documentation. Every test installs the full mock
// layer first, then navigates. No live backend is required — all API
// calls return realistic fake data via page.route(), the same pattern
// used by file-upload.spec.ts.

const here = path.dirname(fileURLToPath(import.meta.url));
const screenshotDir = path.resolve(here, "../../docs/screenshots");

test.beforeAll(() => {
  fs.mkdirSync(screenshotDir, { recursive: true });
});

test.describe("Demo Flow Screenshots", () => {
  test("01 - Login page", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/login");
    await page.waitForLoadState("networkidle");
    await page.screenshot({ path: `${screenshotDir}/01-login-page.png`, fullPage: true });
  });

  test("02 - Signup page", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/signup");
    await page.waitForLoadState("networkidle");
    await page.screenshot({ path: `${screenshotDir}/02-signup-page.png`, fullPage: true });
  });

  test("03 - Drive root with populated folders", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(500);
    // Should show 5 folders with encryption badges (managed + strict-ZK)
    await expect(page.getByText("Engineering").first()).toBeVisible();
    await expect(page.getByText("Legal Contracts").first()).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/03-drive-root-folders.png`, fullPage: true });
  });

  test("04 - Inside folder with subfolders and files", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto(`/drive/folder/${FOLDER_ENGINEERING_ID}`);
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(500);
    // Should show Backend/Frontend subfolders + 5 files
    await expect(page.getByText("architecture-overview.pdf")).toBeVisible();
    await expect(page.getByText("Backend").first()).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/04-folder-contents.png`, fullPage: true });
  });

  test("05 - Create folder dialog (encryption mode picker)", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: /^new folder$/i }).click();
    // Dialog with name input + Managed/Strict-ZK radio
    await expect(page.getByText(/managed encrypted/i).first()).toBeVisible();
    await expect(page.getByText(/strict zero-knowledge/i).first()).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/05-create-folder-dialog.png`, fullPage: true });
  });

  test("06 - Create folder dialog with strict-ZK warning", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: /^new folder$/i }).click();
    // Switch to strict-ZK and capture the warning callout
    await page.getByLabel(/strict zero-knowledge/i).check();
    await expect(page.getByText(/disables server-side previews/i)).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/06-create-folder-strict-zk.png`, fullPage: true });
  });

  test("07 - Search results dropdown", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await page.getByPlaceholder(/search/i).fill("architecture");
    // Debounce in SearchBar is 250ms
    await page.waitForTimeout(500);
    await expect(page.getByText("architecture-overview.pdf").first()).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/07-search-results.png`, fullPage: true });
  });

  test("08 - Create-from-template dialog", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/drive");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: /create from template/i }).click();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${screenshotDir}/08-template-dialog.png`, fullPage: true });
  });

  test("09 - Admin: Users tab", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/admin");
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(500);
    await expect(page.getByText("alice@acmecorp.com")).toBeVisible();
    await expect(page.getByText("Bob Martinez")).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/09-admin-users.png`, fullPage: true });
  });

  test("10 - Admin: Audit log tab", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/admin");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: /^audit$/i }).click();
    await page.waitForTimeout(500);
    await expect(page.getByText(/file\.upload|folder\.create/).first()).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/10-admin-audit-log.png`, fullPage: true });
  });

  test("11 - Admin: Retention policies tab", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/admin");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: /^retention$/i }).click();
    await page.waitForTimeout(500);
    await page.screenshot({ path: `${screenshotDir}/11-admin-retention.png`, fullPage: true });
  });

  test("12 - Admin: Storage usage tab", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/admin");
    await page.waitForLoadState("networkidle");
    await page.getByRole("button", { name: /^storage$/i }).click();
    await page.waitForTimeout(500);
    await expect(page.getByText("alice@acmecorp.com")).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/12-admin-storage.png`, fullPage: true });
  });

  test("13 - Billing page (tier + usage)", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/billing");
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(500);
    await expect(page.getByText(/Plan:\s*business/i)).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/13-billing.png`, fullPage: true });
  });

  test("14 - Placement policy editor", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/admin/placement");
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(500);
    await expect(page.getByRole("heading", { name: /placement policy/i })).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/14-placement-policy.png`, fullPage: true });
  });

  test("15 - Encryption (CMK) page", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/admin/encryption");
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(500);
    await expect(page.getByRole("heading", { name: /encryption/i })).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/15-encryption-cmk.png`, fullPage: true });
  });

  test("16 - KChat rooms admin", async ({ page }) => {
    await installDemoMocks(page);
    await page.goto("/admin/kchat");
    await page.waitForLoadState("networkidle");
    await page.waitForTimeout(500);
    await expect(page.getByRole("heading", { name: /kchat rooms/i })).toBeVisible();
    await expect(page.getByText("general")).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/16-kchat-rooms.png`, fullPage: true });
  });
});
