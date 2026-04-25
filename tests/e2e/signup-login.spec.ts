import { expect, test } from "@playwright/test";

// Deterministic identity per test run so repeated CI passes don't clash
// on the unique email constraint in the users table.
const runID = Date.now();

test("user can sign up and lands in the drive", async ({ page }) => {
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(`E2E Workspace ${runID}`);
  await page.getByLabel(/^email/i).fill(`e2e-${runID}@example.com`);
  await page.getByLabel(/name/i).fill("E2E User");
  await page.getByLabel(/password/i).fill("supersecret123");
  await page.getByRole("button", { name: /sign up/i }).click();
  await expect(page).toHaveURL(/\/drive/);
});

test("logout redirects to /login", async ({ page }) => {
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(`E2E Workspace logout ${runID}`);
  await page.getByLabel(/^email/i).fill(`e2e-logout-${runID}@example.com`);
  await page.getByLabel(/name/i).fill("E2E Logout");
  await page.getByLabel(/password/i).fill("supersecret123");
  await page.getByRole("button", { name: /sign up/i }).click();
  await expect(page).toHaveURL(/\/drive/);
  await page.getByRole("button", { name: /log out/i }).click();
  await expect(page).toHaveURL(/\/login/);
});
