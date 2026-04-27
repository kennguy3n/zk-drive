import { defineConfig, devices } from "@playwright/test";

// CI starts the Go API server on :8080 and points STATIC_DIR at
// `frontend/dist`, so the same origin serves both the SPA and the API.
// Tests therefore navigate against http://localhost:8080 by default and
// rely on baseURL-relative paths (e.g. `/signup`, `/drive`).
const baseURL = process.env.E2E_BASE_URL ?? "http://localhost:8080";

export default defineConfig({
  testDir: "./e2e",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: {
    baseURL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
