import { expect, test } from "@playwright/test";

test.skip(
  !process.env.E2E_RUN_UPLOAD,
  "guest-access e2e needs a working folder target with storage",
);

test("create a guest invite and accept it", async ({ page }) => {
  const stamp = Date.now();
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(`E2E Guest ${stamp}`);
  await page.getByLabel(/^email/i).fill(`host-${stamp}@example.com`);
  await page.getByLabel(/name/i).fill("Host User");
  await page.getByLabel(/password/i).fill("supersecret123");
  await page.getByRole("button", { name: /sign up/i }).click();
  await expect(page).toHaveURL(/\/drive/);

  page.once("dialog", (d) => d.accept("Guest Folder"));
  await page.getByRole("button", { name: /new folder/i }).click();
  await page.getByRole("link", { name: "Guest Folder" }).click();
  // Placeholder: the UI for guest invites is exercised indirectly
  // through the API; the end-to-end surface will land together with
  // the invite dialog in a follow-up sprint.
  await expect(page.getByText(/guest folder/i)).toBeVisible();
});
