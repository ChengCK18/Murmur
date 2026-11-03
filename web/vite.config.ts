import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// In development the gateway runs on :8080; the dev server forwards the API
// and the WebSocket to it, so the app always uses relative URLs.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": "http://localhost:8080",
      "/ws": { target: "ws://localhost:8080", ws: true },
    },
  },
  // MapLibre alone is about 1 MB minified; one chunk is fine for this app.
  build: { chunkSizeWarningLimit: 1500 },
  test: { environment: "node", include: ["src/**/*.test.ts"] },
});
