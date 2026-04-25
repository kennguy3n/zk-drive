import { expect, test } from "@playwright/test";

// Upload-download is skipped by default because it needs a configured
// object storage gateway (MinIO locally, zk-object-fabric in CI). The
// CI job sets E2E_RUN_UPLOAD=1 when the object store is up.
test.skip(
  !process.env.E2E_RUN_UPLOAD,
  "object storage not configured (set E2E_RUN_UPLOAD=1 to enable)",
);

test("upload a file into a new folder", async ({ page }) => {
  const stamp = Date.now();
  await page.goto("/signup");
  await page.getByLabel(/workspace/i).fill(`E2E Upload ${stamp}`);
  await page.getByLabel(/^email/i).fill(`upload-${stamp}@example.com`);
  await page.getByLabel(/name/i).fill("Upload User");
  await page.getByLabel(/password/i).fill("supersecret123");
  await page.getByRole("button", { name: /sign up/i }).click();
  await expect(page).toHaveURL(/\/drive/);

  page.once("dialog", (d) => d.accept("E2E Folder"));
  await page.getByRole("button", { name: /new folder/i }).click();
  await page.getByRole("link", { name: "E2E Folder" }).click();

  const fileChooserPromise = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: /upload/i }).click();
  const chooser = await fileChooserPromise;
  await chooser.setFiles({
    name: "hello.txt",
    mimeType: "text/plain",
    buffer: Buffer.from("hello world"),
  });
  await expect(page.getByText("hello.txt")).toBeVisible();
});
