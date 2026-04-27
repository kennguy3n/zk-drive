import { expect, test } from "@playwright/test";

// uploadFile() in src/api/client.ts orchestrates a three-step dance:
//   1. POST /api/files/upload-url  -> backend mints { upload_url, upload_id, object_key }.
//   2. PUT bytes directly at upload_url.
//   3. POST /api/files/confirm-upload -> backend pins the version as current.
// CI runs the backend without S3_ENDPOINT (no zk-object-fabric gateway),
// so the real /upload-url returns 501. We intercept the three requests
// with page.route() and synthesise the JSON the frontend expects, then
// verify the new file shows up in the list — exactly what a user would
// see after a successful upload.

function uniqueIdentity() {
  const stamp = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  return {
    workspaceName: `E2E Upload ${stamp}`,
    email: `upload-${stamp}@example.com`,
    name: "Upload User",
    password: "supersecret123",
  };
}

test("uploaded file appears in the folder list", async ({ page }) => {
  const id = uniqueIdentity();

  // 1. Sign up and land on the drive.
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(id.workspaceName);
  await page.getByLabel(/your name/i).fill(id.name);
  await page.getByLabel(/^email/i).fill(id.email);
  await page.getByLabel(/password/i).fill(id.password);
  await page.getByRole("button", { name: /create workspace/i }).click();
  await expect(page).toHaveURL(/\/drive$/);

  // 2. Create a folder via the modal so we have somewhere to upload to.
  //    Phase 1 doesn't allow uploads at the workspace root.
  const folderName = `Upload Target ${Date.now()}`;
  await page.getByRole("button", { name: /^new folder$/i }).click();
  await page.getByLabel(/name/i).fill(folderName);
  await page.getByRole("button", { name: /^create$/i }).click();

  // 3. Open the folder and capture its server-assigned ID from the URL.
  await page.getByRole("link", { name: folderName }).click();
  await expect(page).toHaveURL(/\/drive\/folder\//);
  const folderID = page.url().split("/drive/folder/")[1];
  expect(folderID).toBeTruthy();

  // 4. Mock the three upload endpoints. The fake upload_url is opaque to
  //    Playwright's router as long as we match it before letting the
  //    request fly.
  const fakeUploadURL = "http://127.0.0.1:9/zk-drive-mock/upload";
  const fakeFileID = "11111111-1111-1111-1111-111111111111";
  const fakeObjectKey = "mock/object-key";
  const uploadedFilename = "hello.txt";
  const uploadedBody = "hello world";

  await page.route("**/api/files/upload-url", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        upload_url: fakeUploadURL,
        upload_id: fakeFileID,
        object_key: fakeObjectKey,
      }),
    });
  });

  // The browser PUTs directly at upload_url; we short-circuit it so no
  // real network egress happens.
  await page.route(fakeUploadURL, async (route) => {
    await route.fulfill({ status: 200, body: "" });
  });

  let confirmed = false;
  const fakeFile = {
    id: fakeFileID,
    workspace_id: "00000000-0000-0000-0000-000000000000",
    folder_id: folderID,
    name: uploadedFilename,
    size_bytes: uploadedBody.length,
    mime_type: "text/plain",
    current_version_id: "22222222-2222-2222-2222-222222222222",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };

  await page.route("**/api/files/confirm-upload", async (route) => {
    confirmed = true;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ file: fakeFile, version: {} }),
    });
  });

  // After confirm fires, the FileBrowser's onUploaded hook calls
  // refresh() which re-fetches the folder. The real backend doesn't
  // know about our synthesised upload, so we splice the file into the
  // folder GET response only after the confirm step has been hit.
  await page.route(`**/api/folders/${folderID}`, async (route, request) => {
    if (request.method() !== "GET" || !confirmed) {
      await route.fallback();
      return;
    }
    const response = await route.fetch();
    let body: { folder?: unknown; children?: unknown[]; files?: unknown[] };
    try {
      body = await response.json();
    } catch {
      body = {};
    }
    body.files = [...(body.files ?? []), fakeFile];
    await route.fulfill({
      response,
      json: body,
    });
  });

  // 5. Trigger the file picker and feed it an in-memory file. The
  //    UploadButton hides a real <input type="file"> and clicks it,
  //    so Playwright sees a filechooser event.
  const chooserPromise = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: /^upload file$/i }).click();
  const chooser = await chooserPromise;
  await chooser.setFiles({
    name: uploadedFilename,
    mimeType: "text/plain",
    buffer: Buffer.from(uploadedBody),
  });

  // 6. Verify the new row in the file list.
  await expect(page.getByText(uploadedFilename, { exact: false })).toBeVisible();
});
