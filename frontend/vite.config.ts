import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { VitePWA } from "vite-plugin-pwa";

// https://vitejs.dev/config/
export default defineConfig({
  test: {
    // jsdom gives us DOM globals (window, document, WebSocket) for
    // the React + provider unit tests. Excludes the Playwright
    // suite so `npm run test` doesn't try to drive a real browser.
    environment: "jsdom",
    globals: false,
    include: ["src/**/*.test.{ts,tsx}"],
    exclude: ["tests/e2e/**", "node_modules/**"],
    // Initialize i18next synchronously so components using
    // useTranslation() render the English copy in tests instead
    // of the raw "namespace.key" identifiers. Mirrors src/main.tsx.
    setupFiles: ["./src/test/setup.ts"],
  },
  plugins: [
    react(),
    VitePWA({
      registerType: "autoUpdate",
      includeAssets: ["favicon.ico", "apple-touch-icon.png"],
      manifest: {
        name: "ZK Drive",
        short_name: "ZK Drive",
        description: "Secure file storage and collaboration",
        theme_color: "#2563eb",
        background_color: "#ffffff",
        display: "standalone",
        start_url: "/drive",
        icons: [
          { src: "pwa-192x192.png", sizes: "192x192", type: "image/png" },
          { src: "pwa-512x512.png", sizes: "512x512", type: "image/png" },
        ],
      },
      workbox: {
        // Precache the static app shell only. API responses (presigned URLs,
        // CMK URIs, admin data) are intentionally NOT runtime-cached:
        //
        // - For confidential-managed folders (default mode) caching response
        //   bodies would leak server-decrypted content into the browser's
        //   Cache Storage past logout, which directly contradicts the
        //   "decrypted only in the gateway / server memory" boundary that
        //   makes the managed mode honest to call "confidential."
        // - For strict-zero-knowledge folders the gateway never sees
        //   plaintext, but the SPA does once it decrypts via the client
        //   SDK; caching that plaintext (or the presigned URL that resolves
        //   to ciphertext under a session-lived token) would similarly
        //   defeat the contract.
        //
        // Hence: shell only, no runtime API caching. The navigateFallback
        // denylist on `^/api/` belt-and-braces this against a misconfigured
        // Workbox runtime caching rule that might be added later.
        globPatterns: ["**/*.{js,css,html,ico,png,svg}"],
        navigateFallback: "/index.html",
        navigateFallbackDenylist: [/^\/api\//],
        // Pull the Web Push handlers (push / notificationclick) into the
        // generated service worker. Lives in public/ so it's served at the
        // root scope; see public/push-sw.js.
        importScripts: ["/push-sw.js"],
      },
    }),
  ],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});
