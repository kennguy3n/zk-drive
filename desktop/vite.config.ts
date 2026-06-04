import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite config tuned for Tauri (https://v2.tauri.app/start/frontend/vite/).
// The dev server is pinned to a fixed port that `tauri.conf.json`'s
// `build.devUrl` points at; `strictPort` makes a port clash fail loudly
// rather than silently picking another port the Rust host can't find.
const host = process.env.TAURI_DEV_HOST;

export default defineConfig({
  plugins: [react()],
  // Tauri serves the built SPA from `dist/`.
  clearScreen: false,
  server: {
    port: 1420,
    strictPort: true,
    host: host || false,
    hmr: host
      ? {
          protocol: "ws",
          host,
          port: 1421,
        }
      : undefined,
    watch: {
      // Don't watch the Rust source tree from the JS dev server.
      ignored: ["**/src-tauri/**"],
    },
  },
});
