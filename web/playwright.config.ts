import { defineConfig } from "@playwright/test";

// Browser tests against a running stack (make up): the gateway serving the
// built app on MURMUR_URL, with fakegen's 10 drones flying.
export default defineConfig({
  testDir: "e2e",
  timeout: 120_000,
  workers: 1, // the tests restart and stop shared services
  use: {
    baseURL: process.env.MURMUR_URL ?? "http://localhost:8080",
  },
  // Latest Chrome and Firefox (plan open item 8).
  projects: [
    {
      name: "chrome",
      use: {
        channel: "chrome", // the installed Google Chrome
        launchOptions: {
          // MapLibre needs WebGL, which headless Chrome has only with SwiftShader.
          args: ["--enable-unsafe-swiftshader", "--use-angle=swiftshader"],
        },
      },
    },
    // Playwright's own Firefox build: npx playwright install firefox
    { name: "firefox", use: { browserName: "firefox" } },
  ],
});
