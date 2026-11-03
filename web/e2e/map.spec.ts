import { execSync } from "node:child_process";
import { expect, type Page, test } from "@playwright/test";

const compose = process.env.MURMUR_COMPOSE ?? "docker compose -f ../deploy/docker-compose.yml";
const DRONES = 10; // fakegen's -count in the Compose file
// Live drones. Drones from earlier runs stay in Redis and on the map, greyed out.
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

test("the map shows the fleet moving", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  await expect(page.locator(".list li:not(.lost)")).toHaveCount(DRONES);
  await expect(page.getByTestId("banner")).toBeHidden();

  const id = await firstDrone(page);
  await expectMoving(page, id);
  // Clicking a drone in the list selects it on the map.
  await page.locator(`.list li[data-drone-id="${id}"]`).click();
  await expect(page.locator(`.drone[data-drone-id="${id}"]`)).toHaveClass(/selected/);
  expect(errors).toEqual([]);
});

test("Phase 3 check: the browser reconnects after a gateway restart", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  const id = await firstDrone(page);

  execSync(`${compose} restart gateway`, { stdio: "inherit" });
  // The banner shows while the gateway is away, and goes by itself.
  await expect(page.getByTestId("banner")).toBeVisible({ timeout: 10_000 });
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
    await expect(page.locator(".list li:not(.lost)")).toHaveCount(0);
  } finally {
    execSync(`${compose} start fakegen`, { stdio: "inherit" });
  }
  // fakegen restarts on new ports; the rebind rule gives the drones their
  // old IDs, so the same markers come back to life.
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 30_000 });
  const back = await page.locator(LIVE).evaluateAll((els) => els.map((e) => (e as HTMLElement).dataset.droneId));
  expect(back.sort()).toEqual(live.sort());
});
