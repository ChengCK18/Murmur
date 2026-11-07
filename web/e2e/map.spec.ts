import { execSync } from "node:child_process";
import { expect, type Page, test } from "@playwright/test";

const compose = process.env.MURMUR_COMPOSE ?? "docker compose -f ../deploy/docker-compose.yml";
const DRONES = 10; // fakegen's -count in the Compose file
// Live drones. A drone whose link is lost stays on the map, greyed out, until
// it has been silent for the gateway's -stale (10 minutes).
const LIVE = ".drone:not(.lost)";

// position returns where a drone's marker says it is.
async function position(page: Page, id: number): Promise<string> {
  const m = page.locator(`.drone[data-drone-id="${id}"]`);
  return `${await m.getAttribute("data-lat")},${await m.getAttribute("data-lon")}`;
}

async function expectMoving(page: Page, id: number) {
  const before = await position(page, id);
  await expect.poll(() => position(page, id), { timeout: 5_000 }).not.toBe(before);
}

// The lowest live drone ID on the page: IDs depend on what the database has seen.
async function firstDrone(page: Page): Promise<number> {
  const ids = await page.locator(LIVE).evaluateAll((els) => els.map((e) => Number((e as HTMLElement).dataset.droneId)));
  return Math.min(...ids);
}

test("the map draws its tiles", async ({ page }) => {
  // Markers are HTML and appear even when MapLibre cannot render the map, so
  // check the tiles themselves: fetched by MapLibre's worker, and no errors.
  const errors: string[] = [];
  page.on("console", (m) => m.type() === "error" && errors.push(m.text()));
  page.on("pageerror", (e) => errors.push(e.message));
  const tile = page.waitForResponse((r) => r.url().includes("tiles.openfreemap.org/planet/") && r.ok(), { timeout: 20_000 });
  await page.goto("/");
  await tile;
  await page.waitForTimeout(1000);
  expect(errors).toEqual([]);
});

test("the map shows the fleet moving", async ({ page }) => {
  const errors: string[] = [];
  page.on("console", (m) => m.type() === "error" && errors.push(m.text()));
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  await expect(page.locator(".list li[data-drone-id]:not(.lost)")).toHaveCount(DRONES);
  await expect(page.getByTestId("banner")).toBeHidden();
  // No clutter from earlier runs: drones silent for 10 minutes are hidden.
  // (Fails if other drones flew in the last 10 minutes, such as SITL.)
  await expect(page.locator(".drone")).toHaveCount(DRONES);

  const id = await firstDrone(page);
  await expectMoving(page, id);
  // Clicking a drone in the list selects it on the map.
  await page.locator(`.list li[data-drone-id="${id}"]`).click();
  await expect(page.locator(`.drone[data-drone-id="${id}"]`)).toHaveClass(/selected/);
  expect(errors).toEqual([]);
});

// drag scrolls the map by dx pixels, three times, far past the followed area.
async function drag(page: Page, dx: number) {
  const box = (await page.locator(".maplibregl-canvas").boundingBox())!;
  const y = box.y + box.height / 2;
  for (let i = 0; i < 3; i++) {
    const from = dx > 0 ? box.x + 10 : box.x + box.width - 10;
    await page.mouse.move(from, y);
    await page.mouse.down();
    await page.mouse.move(from + dx, y, { steps: 5 });
    await page.waitForTimeout(600); // a pause, so the release throws no inertia
    await page.mouse.up();
  }
  await page.waitForTimeout(300);
}

test("only drones in the map area stay live", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  const ids = await page.locator(LIVE).evaluateAll((els) => els.map((e) => Number((e as HTMLElement).dataset.droneId)));
  const [picked, other] = ids.sort((a, b) => a - b);
  await page.locator(`.list li[data-drone-id="${picked}"]`).click();
  await page.waitForTimeout(800); // the centring animation

  // Scrolled far away: the selected drone stays live, the others do not.
  await drag(page, -900);
  const before = await position(page, other);
  await expectMoving(page, picked);
  await page.waitForTimeout(2500);
  expect(await position(page, other)).toBe(before);

  // Back again: the area covers it, and it moves.
  await drag(page, 900);
  await expectMoving(page, other);
});

test("Phase 3 check: the browser reconnects after a gateway restart", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  const id = await firstDrone(page);

  // Killed, not stopped: it gets no chance to close the WebSocket properly.
  execSync(`${compose} kill gateway`, { stdio: "inherit" });
  // The banner shows while the gateway is away, and goes by itself.
  await expect(page.getByTestId("banner")).toBeVisible({ timeout: 10_000 });
  execSync(`${compose} start gateway`, { stdio: "inherit" });
  await expect(page.getByTestId("banner")).toBeHidden({ timeout: 30_000 });
  // The markers carry on moving, on the fresh snapshot and updates.
  await expectMoving(page, id);
  await expect(page.locator(LIVE)).toHaveCount(DRONES);
});

test("drones whose link is lost turn grey, and recover", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  const live = await page.locator(LIVE).evaluateAll((els) => els.map((e) => (e as HTMLElement).dataset.droneId));
  try {
    execSync(`${compose} stop fakegen`, { stdio: "inherit" });
    // No heartbeat for 3 s: the gateway pushes link lost.
    await expect(page.locator(LIVE)).toHaveCount(0, { timeout: 15_000 });
    await expect(page.locator(".list li[data-drone-id]:not(.lost)")).toHaveCount(0);
  } finally {
    execSync(`${compose} start fakegen`, { stdio: "inherit" });
  }
  // fakegen restarts on new ports; the rebind rule gives the drones their
  // old IDs, so the same markers come back to life.
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 30_000 });
  const back = await page.locator(LIVE).evaluateAll((els) => els.map((e) => (e as HTMLElement).dataset.droneId));
  expect(back.sort()).toEqual(live.sort());
});

test("a drone's flight replays from stored data", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  const id = await firstDrone(page);
  await page.locator(`.list li[data-drone-id="${id}"]`).click();
  await page.getByRole("button", { name: "Load" }).click();

  const info = page.getByTestId("replay-point");
  await expect(info).toBeVisible();
  expect(Number(await info.getAttribute("data-points"))).toBeGreaterThan(5);
  const ghost = page.locator(".replay-ghost");
  const where = async () => `${await ghost.getAttribute("data-lat")},${await ghost.getAttribute("data-lon")}`;

  // Scrub from the start to the end: the ghost moves, and says what it was doing.
  const scrubber = page.getByLabel("Replay time");
  await scrubber.fill(await scrubber.getAttribute("min") as string);
  const start = await where();
  await scrubber.fill(await scrubber.getAttribute("max") as string);
  await expect.poll(where).not.toBe(start);
  await expect(info).toContainText(/AUTO|RTL|LAND|GUIDED/);

  // Play runs forward on its own.
  await scrubber.fill(await scrubber.getAttribute("min") as string);
  const before = await scrubber.inputValue();
  await page.getByRole("button", { name: "Play" }).click();
  await expect.poll(() => scrubber.inputValue()).not.toBe(before);
  await page.getByRole("button", { name: "Close" }).click();
  await expect(ghost).toHaveCount(0);
});
