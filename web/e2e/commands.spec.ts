import { execSync } from "node:child_process";
import { expect, type Page, test } from "@playwright/test";

const compose = process.env.MURMUR_COMPOSE ?? "docker compose -f ../deploy/docker-compose.yml";
const DRONES = 10;
const LIVE = ".drone:not(.lost)";
const PASSWORD = "murmur"; // the gateway's -password in the Compose file

async function selectADrone(page: Page): Promise<number> {
  await page.goto("/");
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  const ids = await page.locator(LIVE).evaluateAll((els) => els.map((e) => Number((e as HTMLElement).dataset.droneId)));
  const id = Math.min(...ids);
  await page.locator(`.list li[data-drone-id="${id}"]`).click();
  await expect(page.getByRole("heading", { name: `Drone ${id}` })).toBeVisible();
  return id;
}

async function logIn(page: Page, password = PASSWORD) {
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Log in" }).click();
}

const newest = (page: Page) => page.getByRole("list", { name: "Commands" }).locator("li").first();

// These tests change what the shared drones are doing, so put them back: past
// the link timeout, the fleet returns on its routes with the same IDs.
test.afterEach(async ({ page }) => {
  execSync(`${compose} stop fakegen`, { stdio: "inherit" });
  await page.waitForTimeout(4000);
  execSync(`${compose} start fakegen`, { stdio: "inherit" });
});

test("sending commands takes a login", async ({ page }) => {
  await selectADrone(page);
  await expect(page.getByRole("button", { name: "Land", exact: true })).toBeHidden();
  await expect(page.getByText("Demo login: demo / murmur")).toBeVisible(); // a stranger must not have to guess
  await logIn(page, "wrong");
  await expect(page.getByText("wrong username or password")).toBeVisible();
  await expect(page.getByRole("button", { name: "Land", exact: true })).toBeHidden();

  await logIn(page);
  await expect(page.getByText("Logged in as demo")).toBeVisible();
  await expect(page.getByRole("button", { name: "Land", exact: true })).toBeVisible();

  await page.reload(); // the session is a cookie, so it outlives the page
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  await page.locator(".list li[data-drone-id]").first().click();
  await expect(page.getByText("Logged in as demo")).toBeVisible();

  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page.getByRole("button", { name: "Land", exact: true })).toBeHidden();
});

test("a command runs on the drone and every step shows", async ({ page }) => {
  const id = await selectADrone(page);
  await logIn(page);
  // Only the line of drone state: "GUIDED" is in the mode list too, and "armed"
  // in the refusal above. It changes on the drone's next heartbeat, 1 Hz.
  const state = page.getByRole("region", { name: `Drone ${id}` }).locator(".state");

  // The platform refuses this itself, with a reason, and never sends it.
  await page.getByRole("button", { name: "Takeoff" }).click();
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(newest(page)).toHaveAttribute("data-status", "rejected");
  await expect(newest(page)).toContainText("Takeoff needs the drone armed and in GUIDED mode");

  await page.getByRole("button", { name: "Land", exact: true }).click();
  await expect(newest(page)).toHaveAttribute("data-status", "accepted");
  await expect(state).toContainText("LAND");

  // Disarmed on landing, 20 to 60 m below: the drone itself reports it.
  await expect(state).toContainText("· disarmed ·", { timeout: 60_000 });
  await expect(state).toContainText("· 0.0 m ·");

  // Arming asks first, and Cancel means nothing is sent.
  const commands = await page.getByRole("list", { name: "Commands" }).locator("li").count();
  await page.getByRole("button", { name: "Arm", exact: true }).click();
  await expect(page.getByRole("alertdialog")).toContainText(`Arm drone ${id}?`);
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByRole("alertdialog")).toBeHidden();
  await expect(page.getByRole("list", { name: "Commands" }).locator("li")).toHaveCount(commands);

  await page.getByRole("button", { name: "Arm", exact: true }).click();
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(newest(page)).toHaveAttribute("data-status", "accepted");
  await expect(state).toContainText("· armed ·");

  // The way back up: GUIDED, then takeoff, and the height shows on the panel.
  await page.getByRole("button", { name: "Set mode" }).click();
  await expect(state).toContainText("GUIDED ·");
  await page.getByLabel("Takeoff altitude in metres").fill("5");
  await page.getByRole("button", { name: "Takeoff" }).click();
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(newest(page)).toHaveAttribute("data-status", "accepted");
  await expect(state).toContainText("· 5.0 m ·", { timeout: 15_000 });
});

test("the command list survives a gateway restart", async ({ page }) => {
  await selectADrone(page);
  await logIn(page);
  await page.getByRole("button", { name: "Return to launch" }).click();
  await expect(newest(page)).toHaveAttribute("data-status", "accepted");

  execSync(`${compose} kill gateway`, { stdio: "inherit" });
  await expect(page.getByTestId("banner")).toBeVisible({ timeout: 10_000 });
  execSync(`${compose} start gateway`, { stdio: "inherit" });
  await expect(page.getByTestId("banner")).toBeHidden({ timeout: 30_000 });

  await expect(newest(page)).toContainText("Return to launch");
  await expect(newest(page)).toHaveAttribute("data-status", "accepted");
  await expect(page.getByText("Logged in as demo")).toBeVisible();
});

test("a fleet command ends with a result for every drone", async ({ page }) => {
  await selectADrone(page);
  await logIn(page);
  await page.getByRole("button", { name: "Return all to launch" }).click();
  await expect(page.getByRole("alertdialog", { name: "Confirm fleet command" })).toContainText(`all ${DRONES} live drones`);
  await page.getByRole("button", { name: "Confirm" }).click();

  const summary = page.getByTestId("fleet-summary");
  await expect(summary).toHaveAttribute("data-waiting", "0", { timeout: 5_000 });
  await expect(summary).toHaveText(`Return to launch: ${DRONES} accepted`);
  await expect(page.locator(".list li[data-drone-id]", { hasText: "RTL" })).toHaveCount(DRONES, { timeout: 15_000 });
});

test("a mission is planned on the map, uploaded and flown", async ({ page }) => {
  const id = await selectADrone(page);
  await logIn(page);
  await expect(page.getByTestId("mission-state")).toBeVisible();

  // Three points on the map, away from the panel.
  await page.getByRole("button", { name: "Plan on map" }).click();
  const box = (await page.locator(".maplibregl-canvas").boundingBox())!;
  for (const [x, y] of [[0.2, 0.3], [0.3, 0.6], [0.4, 0.3]]) {
    await page.mouse.click(box.x + box.width * x, box.y + box.height * y);
  }
  await expect(page.locator(".waypoint")).toHaveCount(3);
  await expect(page.getByText("3 so far")).toBeVisible();

  await page.getByLabel("Mission altitude in metres").fill("25"); // unlike any mission an earlier run left
  await page.getByRole("button", { name: "Upload" }).click();
  await expect(page.getByTestId("mission-state")).toHaveText("3 waypoints at 25 m: uploaded", { timeout: 10_000 });
  await expect(newest(page)).toContainText("Mission upload");
  await expect(newest(page)).toHaveAttribute("data-status", "accepted");
  await expect(page.locator(".waypoint")).toHaveCount(3); // still drawn, now as the drone's

  await page.getByRole("button", { name: "Start mission" }).click();
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(newest(page)).toContainText("Start mission");
  await expect(newest(page)).toHaveAttribute("data-status", "accepted");

  // It leaves its route for the first waypoint, in AUTO.
  const marker = page.locator(`.drone[data-drone-id="${id}"]`);
  const at = async () => [Number(await marker.getAttribute("data-lat")), Number(await marker.getAttribute("data-lon"))];
  const start = await at();
  await expect.poll(async () => {
    const [lat, lon] = await at();
    return Math.hypot(lat - start[0], lon - start[1]);
  }, { timeout: 15_000 }).toBeGreaterThan(0.0001);
});

test("alerts show for a geofence breach and a lost link, and clear", async ({ page }) => {
  await selectADrone(page);
  await logIn(page);
  const alerts = page.getByRole("region", { name: "Alerts" });

  // A small triangle in a corner of the map, which no drone is inside.
  await page.getByRole("button", { name: "Draw geofence" }).click();
  const box = (await page.locator(".maplibregl-canvas").boundingBox())!;
  for (const [x, y] of [[0.05, 0.9], [0.1, 0.9], [0.07, 0.95]]) {
    await page.mouse.click(box.x + box.width * x, box.y + box.height * y);
  }
  await page.getByRole("button", { name: "Save geofence" }).click();
  await expect(page.locator(".map")).toHaveAttribute("data-fence-points", "3");
  try {
    await expect(alerts.locator('li[data-kind="geofence"]')).toHaveCount(DRONES, { timeout: 10_000 });
    await expect(page.locator(".drone.alert")).toHaveCount(DRONES);
  } finally {
    await page.getByRole("button", { name: "Remove geofence" }).click();
  }
  await expect(alerts.locator('li[data-kind="geofence"]')).toHaveCount(0, { timeout: 10_000 });

  // The fleet goes quiet: link lost for every drone, cleared when it returns.
  execSync(`${compose} stop fakegen`, { stdio: "inherit" });
  await expect(alerts.locator('li[data-kind="link_lost"]')).toHaveCount(DRONES, { timeout: 15_000 });
  execSync(`${compose} start fakegen`, { stdio: "inherit" });
  await expect(alerts.locator('li[data-kind="link_lost"]')).toHaveCount(0, { timeout: 30_000 });
});
