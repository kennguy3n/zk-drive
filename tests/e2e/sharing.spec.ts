import { expect, test } from "@playwright/test";

test.skip(
  !process.env.E2E_RUN_UPLOAD,
  "sharing e2e needs object storage to create a shareable file",
);

test("create a share link and resolve it anonymously", async ({ browser, page }) => {
  const stamp = Date.now();
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(`E2E Share ${stamp}`);
  await page.getByLabel(/^email/i).fill(`share-${stamp}@example.com`);
  await page.getByLabel(/name/i).fill("Share User");
  await page.getByLabel(/password/i).fill("supersecret123");
  await page.getByRole("button", { name: /sign up/i }).click();
  await expect(page).toHaveURL(/\/drive/);

  page.once("dialog", (d) => d.accept("Share Folder"));
  await page.getByRole("button", { name: /new folder/i }).click();
  await page.getByRole("link", { name: "Share Folder" }).click();
  await page.getByRole("button", { name: /share/i }).first().click();
  await page.getByRole("button", { name: /create link/i }).click();
  const tokenInput = page.locator('input[readonly]').first();
  const token = await tokenInput.inputValue();
  expect(token).toMatch(/http/);

  const anon = await browser.newContext();
  const anonPage = await anon.newPage();
  await anonPage.goto(token);
  await expect(anonPage.getByText(/share folder/i)).toBeVisible();
});
