import { expect, test } from "@playwright/test";

// Workspace owners are admin-by-default (see api/auth/handler.go), so a
// freshly-signed-up user can reach both /admin and /billing. These tests
// only assert that each page's structural surface renders — they don't
// touch backend data because the Phase 1 admin/billing endpoints might
// 401 / 403 / 5xx on a brand-new tenant, and we want the navigation gate
// to stand on its own.

function uniqueIdentity(prefix: string) {
  const stamp = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  return {
    workspaceName: `E2E ${prefix} ${stamp}`,
    email: `${prefix}-${stamp}@example.com`,
    name: `${prefix} Admin`,
    password: "supersecret123",
  };
}

async function signupAdmin(page: import("@playwright/test").Page, prefix: string) {
  const id = uniqueIdentity(prefix);
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(id.workspaceName);
  await page.getByLabel(/your name/i).fill(id.name);
  await page.getByLabel(/^email/i).fill(id.email);
  await page.getByLabel(/password/i).fill(id.password);
  await page.getByRole("button", { name: /create workspace/i }).click();
  await expect(page).toHaveURL(/\/drive$/);
}

test.describe("navigation", () => {
  test("admin link routes to /admin and the page renders", async ({ page }) => {
    await signupAdmin(page, "Admin");

    await page.getByRole("link", { name: /^admin$/i }).click();
    await expect(page).toHaveURL(/\/admin$/);
    // AdminPage renders an "Admin" header and a tab strip with "Users"
    // selected by default. Use the tab button as our presence check —
    // it's stable regardless of what the underlying APIs return.
    await expect(page.getByRole("button", { name: /^users$/i })).toBeVisible();
  });

  test("billing link routes to /billing and the page renders", async ({ page }) => {
    await signupAdmin(page, "Billing");

    await page.getByRole("link", { name: /^billing$/i }).click();
    await expect(page).toHaveURL(/\/billing$/);
    // BillingPage shows an "Admin" / "Back to drive" navigation row
    // immediately, before the usage fetch resolves. The page heading is
    // the most stable presence check.
    await expect(page.getByRole("heading", { name: /^billing$/i })).toBeVisible();
  });

  test("placement page is reachable from /admin/placement", async ({ page }) => {
    await signupAdmin(page, "Placement");

    await page.goto("/admin/placement");
    await expect(page).toHaveURL(/\/admin\/placement$/);
    // PlacementPage always renders its own heading; data may or may not
    // load depending on environment configuration.
    await expect(page.locator("h1, h2, h3").first()).toBeVisible();
  });
});
