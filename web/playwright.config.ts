import { defineConfig } from "@playwright/test";

// Browser tests against a running stack (make up): the gateway serving the
// built app on MURMUR_URL, with fakegen's 10 drones flying.
export default defineConfig({
  testDir: "e2e",
  timeout: 120_000,
  workers: 1, // the tests restart and stop shared services
  use: {
    baseURL: process.env.MURMUR_URL ?? "http://localhost:8080",
    channel: "chrome", // the installed Google Chrome; no browser download
    launchOptions: {
      // MapLibre needs WebGL, which headless Chrome has only with SwiftShader.
      args: ["--enable-unsafe-swiftshader", "--use-angle=swiftshader"],
    },
  },
});
