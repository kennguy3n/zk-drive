import { expect, test } from "@playwright/test";

test("admin can see audit log and storage usage", async ({ page }) => {
  const stamp = Date.now();
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(`E2E Admin ${stamp}`);
  await page.getByLabel(/^email/i).fill(`admin-${stamp}@example.com`);
  await page.getByLabel(/name/i).fill("Admin User");
  await page.getByLabel(/password/i).fill("supersecret123");
  await page.getByRole("button", { name: /sign up/i }).click();
  await expect(page).toHaveURL(/\/drive/);

  // Signup owner is admin-by-default, so the Admin link should be
  // visible in the drive header.
  await page.getByRole("link", { name: /admin/i }).click();
  await expect(page).toHaveURL(/\/admin/);
  await expect(page.getByRole("heading", { name: /users/i })).toBeVisible();
  await page.getByRole("button", { name: /audit/i }).click();
  await expect(page.getByRole("heading", { name: /audit log/i })).toBeVisible();
  await page.getByRole("button", { name: /storage/i }).click();
  await expect(page.getByRole("heading", { name: /storage/i })).toBeVisible();
});

test("admin can view billing page", async ({ page }) => {
  const stamp = Date.now();
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(`E2E Billing ${stamp}`);
  await page.getByLabel(/^email/i).fill(`billing-${stamp}@example.com`);
  await page.getByLabel(/name/i).fill("Billing Admin");
  await page.getByLabel(/password/i).fill("supersecret123");
  await page.getByRole("button", { name: /sign up/i }).click();
  await expect(page).toHaveURL(/\/drive/);
  await page.getByRole("link", { name: /billing/i }).click();
  await expect(page).toHaveURL(/\/billing/);
  await expect(page.getByText(/plan:/i)).toBeVisible();
});
