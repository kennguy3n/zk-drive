import { expect, test } from "@playwright/test";

// Each test gets its own identity so the unique-email constraint in the
// `users` table doesn't collide across retries or parallel runs.
function uniqueIdentity(prefix: string) {
  const stamp = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  return {
    workspaceName: `E2E ${prefix} ${stamp}`,
    email: `${prefix}-${stamp}@example.com`,
    name: `${prefix} User`,
    password: "supersecret123",
  };
}

test.describe("auth", () => {
  test("signup creates a workspace and lands on the drive", async ({ page }) => {
    const id = uniqueIdentity("signup");

    await page.goto("/signup");
    await expect(page.getByRole("heading", { name: /create a zk-drive workspace/i })).toBeVisible();

    await page.getByLabel(/workspace/i).fill(id.workspaceName);
    await page.getByLabel(/your name/i).fill(id.name);
    await page.getByLabel(/^email/i).fill(id.email);
    await page.getByLabel(/password/i).fill(id.password);
    await page.getByRole("button", { name: /create workspace/i }).click();

    await expect(page).toHaveURL(/\/drive$/);
    await expect(page.getByRole("button", { name: /log out/i })).toBeVisible();
  });

  test("logout then login returns to the drive", async ({ page }) => {
    const id = uniqueIdentity("login");

    // Sign up first so we have a known account to log into.
    await page.goto("/signup");
    await page.getByLabel(/workspace/i).fill(id.workspaceName);
    await page.getByLabel(/your name/i).fill(id.name);
    await page.getByLabel(/^email/i).fill(id.email);
    await page.getByLabel(/password/i).fill(id.password);
    await page.getByRole("button", { name: /create workspace/i }).click();
    await expect(page).toHaveURL(/\/drive$/);

    // Log out — the header button should bounce us to /login.
    await page.getByRole("button", { name: /log out/i }).click();
    await expect(page).toHaveURL(/\/login$/);

    // Log back in with the same credentials.
    await page.getByLabel(/^email/i).fill(id.email);
    await page.getByLabel(/password/i).fill(id.password);
    await page.getByRole("button", { name: /sign in/i }).click();

    await expect(page).toHaveURL(/\/drive$/);
    await expect(page.getByRole("button", { name: /log out/i })).toBeVisible();
  });
});
