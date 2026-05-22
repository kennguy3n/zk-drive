import { expect, test } from "@playwright/test";

// uniqueIdentity mirrors the helper in auth.spec.ts / navigation.spec.ts
// — kept inline to avoid coupling the privacy e2e to those files.
function uniqueIdentity(prefix: string) {
  const stamp = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  return {
    workspaceName: `E2E ${prefix} ${stamp}`,
    email: `${prefix}-${stamp}@example.com`,
    name: `${prefix} User`,
    password: "supersecret123",
  };
}

async function signupAndLand(page: import("@playwright/test").Page, prefix: string) {
  const id = uniqueIdentity(prefix);
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(id.workspaceName);
  await page.getByLabel(/your name/i).fill(id.name);
  await page.getByLabel(/^email/i).fill(id.email);
  await page.getByLabel(/password/i).fill(id.password);
  await page.getByRole("button", { name: /create workspace/i }).click();
  await expect(page).toHaveURL(/\/drive$/);
}

test.describe("privacy", () => {
  test("Privacy link in the drive header opens the privacy page", async ({ page }) => {
    await signupAndLand(page, "Privacy");

    await page.getByRole("link", { name: /^privacy$/i }).click();
    await expect(page).toHaveURL(/\/drive\/privacy$/);
    await expect(
      page.getByRole("heading", { name: /how your data is protected/i }),
    ).toBeVisible();
  });

  test("privacy page documents both modes honestly", async ({ page }) => {
    await signupAndLand(page, "PrivacyCopy");
    await page.goto("/drive/privacy");

    // Confidential-managed section must say it is NOT zero-knowledge so
    // the user knows the server can read plaintext under the default.
    await expect(
      page.getByRole("heading", { name: /confidential managed/i }),
    ).toBeVisible();
    await expect(page.getByText(/not.*zero-knowledge/i).first()).toBeVisible();
    await expect(page.getByText(/server.*read plaintext/i).first()).toBeVisible();

    // Strict-ZK section must surface the trade-off (no previews, search,
    // virus scanning) so the user understands what they give up.
    await expect(
      page.getByRole("heading", { name: /strict zero-knowledge/i }),
    ).toBeVisible();
    await expect(page.getByText(/disabled/i).first()).toBeVisible();
  });

  test("create-folder dialog links to /drive/privacy", async ({ page }) => {
    await signupAndLand(page, "Dialog");

    await page.getByRole("button", { name: /new folder/i }).click();
    // The fieldset legend is now "Privacy mode" (was "Encryption mode").
    await expect(page.getByText(/privacy mode/i)).toBeVisible();
    await expect(page.getByText(/confidential managed/i)).toBeVisible();

    // Pick strict-ZK so the irreversibility warning shows up — that
    // copy is the honest disclosure required by PROPOSAL §3.3.
    // Use the input selector directly so body-copy edits don't break
    // the test (the label text contains the mode-trade-off prose).
    await page.locator('input[name="encmode"][value="strict_zk"]').check();
    await expect(page.getByRole("alert")).toContainText(/irreversible/i);

    // The "Learn how each mode works" link opens the privacy explainer
    // in a new tab so the in-flight folder name + mode-radio selection
    // in this dialog survive the click. Waiting on the `popup` event
    // is how Playwright observes target=_blank navigations: the
    // original `page` stays on /drive (the dialog is still mounted),
    // and the new tab gets exposed as `popup`.
    const popupPromise = page.waitForEvent("popup");
    await page.getByRole("link", { name: /learn how each mode works/i }).click();
    const popup = await popupPromise;
    await popup.waitForLoadState();
    await expect(popup).toHaveURL(/\/drive\/privacy$/);
    // The original page (with the dialog still open) must still be
    // on /drive — proving the click did NOT unmount FileBrowserPage.
    await expect(page).toHaveURL(/\/drive$/);
    await expect(page.getByText(/privacy mode/i)).toBeVisible();
  });
});
