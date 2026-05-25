import { expect, test } from "@playwright/test";

// uniqueIdentity / signupAndLand mirror the helper used by the
// other e2e specs; kept inline so the documents suite has no
// inter-spec coupling.
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

// Helper that creates a folder via the create-folder dialog and
// returns the URL of the new folder page (which is the path the
// app navigates to after creation).
async function createFolder(
  page: import("@playwright/test").Page,
  name: string,
  privacyMode: "managed_encrypted" | "strict_zk",
) {
  await page.getByRole("button", { name: /new folder/i }).click();
  // The dialog labels its input "Name" via a <label> wrapping a
  // <span>Name</span> + <input>; getByLabel matches that. The
  // strict_zk radio shows an irreversibility warning panel but
  // does NOT require a separate confirm checkbox before submit.
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.locator(`input[name="encmode"][value="${privacyMode}"]`).check();
  await page.getByRole("button", { name: /^create$/i }).click();
  // The dialog closes; navigate into the new folder via the
  // folder tree (the tree links carry the folder name as
  // their accessible name).
  await page.getByRole("link", { name }).first().click();
  await expect(page).toHaveURL(/\/drive\/folder\//);
}

test.describe("documents", () => {
  test("Documents button in folder header opens an empty list", async ({ page }) => {
    await signupAndLand(page, "DocsEmpty");
    await createFolder(page, "Confidential Folder", "managed_encrypted");

    await page.getByRole("link", { name: /documents/i }).click();
    await expect(page).toHaveURL(/\/drive\/folder\/[^/]+\/documents$/);
    await expect(
      page.getByRole("heading", { name: /confidential folder.*documents/i }),
    ).toBeVisible();
    await expect(page.getByText(/no documents in this folder yet/i)).toBeVisible();
    await expect(page.getByRole("button", { name: /new document/i })).toBeVisible();
  });

  test("New document dialog gates rich modes in strict_zk folders", async ({ page }) => {
    await signupAndLand(page, "DocsStrict");
    await createFolder(page, "Zero Knowledge Folder", "strict_zk");

    await page.getByRole("link", { name: /documents/i }).click();
    await page.getByRole("button", { name: /new document/i }).click();

    // Markdown stays enabled; rich and rich+presence are disabled
    // with the strict_zk-specific tooltip.
    await expect(page.locator('input[type="radio"][value="markdown"]')).toBeEnabled();
    await expect(page.locator('input[type="radio"][value="rich"]')).toBeDisabled();
    await expect(
      page.locator('input[type="radio"][value="rich_presence"]'),
    ).toBeDisabled();
  });

  test("New document in confidential folder lands in editor with all modes available", async ({
    page,
  }) => {
    await signupAndLand(page, "DocsRich");
    await createFolder(page, "Rich Folder", "managed_encrypted");

    await page.getByRole("link", { name: /documents/i }).click();
    await page.getByRole("button", { name: /new document/i }).click();

    // Scope to the dialog so we don't accidentally match controls
    // on the underlying documents list page or the header.
    const dialog = page.getByRole("dialog");

    // Every mode is enabled in a managed_encrypted folder.
    await expect(dialog.locator('input[type="radio"][value="markdown"]')).toBeEnabled();
    await expect(dialog.locator('input[type="radio"][value="rich"]')).toBeEnabled();
    await expect(
      dialog.locator('input[type="radio"][value="rich_presence"]'),
    ).toBeEnabled();

    // Pick rich+presence and create — wait for the POST /documents
    // response explicitly so we can both prove the API succeeded AND
    // grab the new document id without racing the SPA navigation.
    await dialog.locator('input[type="radio"][value="rich_presence"]').check();
    await dialog.getByLabel("Name", { exact: true }).fill("Design Doc");
    const createResp = page.waitForResponse(
      (r) => r.url().endsWith("/api/documents") && r.request().method() === "POST",
    );
    await dialog.getByRole("button", { name: /^create$/i }).click();
    const resp = await createResp;
    expect(resp.status(), `create failed: ${await resp.text()}`).toBe(200);
    const created = (await resp.json()) as { id: string; collab_mode: string };
    expect(created.collab_mode).toBe("rich_presence");

    // After the POST resolves the page navigates client-side to the
    // editor route. Wait for the URL to settle.
    await expect(page).toHaveURL(new RegExp(`/drive/document/${created.id}$`));
    await expect(page.getByRole("heading", { name: /design doc/i })).toBeVisible();
    // The connection chip cycles connecting → connected; either
    // text qualifies as "the editor mounted and is talking to the
    // collab WS".
    await expect(
      page.getByText(/connecting|live|reconnecting/i).first(),
    ).toBeVisible();
  });
});
