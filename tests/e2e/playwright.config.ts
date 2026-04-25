import { defineConfig } from "@playwright/test";

// Playwright suite driving the full stack end-to-end. Tests expect the
// API server on :8080 and the frontend on :5173 (vite dev server).
// CI runs `docker compose up -d` first so both services are reachable.
export default defineConfig({
  testDir: ".",
  timeout: 30_000,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? "github" : "list",
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:5173",
    trace: "on-first-retry",
    screenshot: "only-on-failure",
  },
});
