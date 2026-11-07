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
  await expect(page.getByRole("button", { name: "Land" })).toBeHidden();
  await logIn(page, "wrong");
  await expect(page.getByText("wrong username or password")).toBeVisible();
  await expect(page.getByRole("button", { name: "Land" })).toBeHidden();

  await logIn(page);
  await expect(page.getByText("Logged in as demo")).toBeVisible();
  await expect(page.getByRole("button", { name: "Land" })).toBeVisible();

  await page.reload(); // the session is a cookie, so it outlives the page
  await expect(page.locator(LIVE)).toHaveCount(DRONES, { timeout: 20_000 });
  await page.locator(".list li").first().click();
  await expect(page.getByText("Logged in as demo")).toBeVisible();

  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page.getByRole("button", { name: "Land" })).toBeHidden();
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

  await page.getByRole("button", { name: "Land" }).click();
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
