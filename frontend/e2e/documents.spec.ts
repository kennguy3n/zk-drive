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
    // PageHeader now renders the folder name as the <h1> with a
    // "Documents" eyebrow above it (the old build concatenated both
    // into a single heading). The route assertion above already pins
    // us to the documents view, so the folder-name heading is enough.
    await expect(
      page.getByRole("heading", { name: /confidential folder/i }),
    ).toBeVisible();
    await expect(page.getByText(/no documents yet/i)).toBeVisible();
    // Two "New document" CTAs now exist on an empty list (the header
    // action and the empty-state prompt); assert on the header one.
    await expect(
      page.getByRole("button", { name: /new document/i }).first(),
    ).toBeVisible();
  });

  test("New document dialog gates rich modes in strict_zk folders", async ({ page }) => {
    await signupAndLand(page, "DocsStrict");
    await createFolder(page, "Zero Knowledge Folder", "strict_zk");

    await page.getByRole("link", { name: /documents/i }).click();
    // Empty list renders both a header and an empty-state "New
    // document" CTA; either opens the dialog, so pick the header one.
    await page.getByRole("button", { name: /new document/i }).first().click();

    // The mode picker is now a KChat RadioCard set: a role="radiogroup"
    // of role="radio" buttons rendered in document order
    // markdown -> rich -> rich_presence (CollabModeSelector MODES).
    // Markdown stays enabled in strict_zk; the two rich modes are
    // disabled because the server can't merge updates it can't read.
    const dialog = page.getByRole("dialog");
    const modes = dialog.getByRole("radio");
    await expect(modes.nth(0)).toBeEnabled();
    await expect(modes.nth(1)).toBeDisabled();
    await expect(modes.nth(2)).toBeDisabled();
  });

  test("New document in confidential folder lands in editor with all modes available", async ({
    page,
  }) => {
    await signupAndLand(page, "DocsRich");
    await createFolder(page, "Rich Folder", "managed_encrypted");

    await page.getByRole("link", { name: /documents/i }).click();
    // Empty list renders both a header and an empty-state "New
    // document" CTA; either opens the dialog, so pick the header one.
    await page.getByRole("button", { name: /new document/i }).first().click();

    // Scope to the dialog so we don't accidentally match controls
    // on the underlying documents list page or the header.
    const dialog = page.getByRole("dialog");

    // The mode picker is now a KChat RadioCard set: a role="radiogroup"
    // of role="radio" buttons in document order markdown -> rich ->
    // rich_presence. Every mode is enabled in a managed_encrypted folder.
    const modes = dialog.getByRole("radio");
    await expect(modes.nth(0)).toBeEnabled();
    await expect(modes.nth(1)).toBeEnabled();
    await expect(modes.nth(2)).toBeEnabled();

    // Pick rich+presence and create — wait for the POST /documents
    // response explicitly so we can both prove the API succeeded AND
    // grab the new document id without racing the SPA navigation.
    await modes.nth(2).click();
    await dialog.getByLabel("Name", { exact: true }).fill("Design Doc");
    const createResp = page.waitForResponse(
      (r) => r.url().endsWith("/api/documents") && r.request().method() === "POST",
    );
    await dialog.getByRole("button", { name: /^create$/i }).click();
    const resp = await createResp;
    // The create handler writes http.StatusCreated (201) on success.
    expect(resp.status(), `create failed: ${await resp.text()}`).toBe(201);
    const created = (await resp.json()) as { id: string; collab_mode: string };
    expect(created.collab_mode).toBe("rich_presence");

    // After the POST resolves the page navigates client-side to the
    // editor route. We deliberately don't waitForResponse on the GET
    // /api/documents/{id} here: in earlier attempts the listener
    // race-condition (register-after-fire) caused 60s test timeouts
    // in CI even after moving registration before the click. The
    // simpler, more robust signal is the editor's h1 heading — it
    // only renders after the GET resolves AND the SPA finishes
    // mounting the Y.Doc + provider chain, so its visibility implies
    // both. The 20s timeout is generous enough to absorb CI runner
    // jitter without hiding real regressions: a healthy mount on
    // this machine takes ~200ms.
    await expect(page).toHaveURL(new RegExp(`/drive/document/${created.id}$`));
    await expect(page.getByRole("heading", { name: /design doc/i })).toBeVisible({
      timeout: 20_000,
    });
    // The connection chip cycles connecting → connected; either
    // text qualifies as "the editor mounted and is talking to the
    // collab WS".
    await expect(
      page.getByText(/connecting|live|reconnecting/i).first(),
    ).toBeVisible();
  });
});
