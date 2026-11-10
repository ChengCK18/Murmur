// Records the README's demo GIF and screenshots from the running stack.
//
//   ./start.sh && cd web && npm run record
//
// It drives the map as a visitor would: select a drone, log in, plan and fly
// a mission, then send the whole fleet home. Playwright records the page and
// ffmpeg turns the recording into docs/img/demo.gif. It also saves
// docs/img/map.png and docs/img/grafana.png. It changes what the drones are
// doing, so afterwards stop and then start fakegen for a fresh fleet ("restart"
// can give it a new address, and so new drone IDs). DEMO_DRONE picks which
// drone in the list to fly: one with no stored mission looks cleanest.
import { execFileSync } from "node:child_process";
import { mkdtempSync, renameSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { chromium } from "@playwright/test";

const MAP = process.env.MURMUR_URL ?? "http://localhost:8080";
const GRAFANA = process.env.GRAFANA_URL ?? "http://localhost:3000";
const OUT = new URL("../../docs/img/", import.meta.url).pathname;
const SIZE = { width: 1280, height: 720 };

const browser = await chromium.launch({
  channel: "chrome",
  args: ["--use-gl=angle", "--use-angle=swiftshader", "--enable-unsafe-swiftshader"],
});
const videoDir = mkdtempSync(join(tmpdir(), "murmur-video-"));
const context = await browser.newContext({ viewport: SIZE, recordVideo: { dir: videoDir, size: SIZE } });
const recording = Date.now();
const page = await context.newPage();

// Playwright's video has no pointer, so draw one: a viewer has to see what
// was clicked.
await page.addInitScript(() => {
  addEventListener("DOMContentLoaded", () => {
    const dot = document.createElement("div");
    dot.style.cssText =
      "position:fixed;z-index:99999;width:18px;height:18px;margin:-9px 0 0 -9px;border-radius:50%;" +
      "pointer-events:none;border:2px solid #fff;background:rgb(255 255 255 / 0.25);" +
      "box-shadow:0 0 10px rgb(0 0 0 / 0.6);transition:transform 0.12s;left:-40px;top:-40px";
    document.body.append(dot);
    addEventListener("mousemove", (e) => {
      dot.style.left = `${e.clientX}px`;
      dot.style.top = `${e.clientY}px`;
    });
    addEventListener("mousedown", () => (dot.style.transform = "scale(0.6)"));
    addEventListener("mouseup", () => (dot.style.transform = ""));
  });
});

let mouse = { x: SIZE.width / 2, y: SIZE.height / 2 };
async function moveTo(x, y) {
  await page.mouse.move(x, y, { steps: 18 });
  mouse = { x, y };
}
async function clickAt(x, y) {
  await moveTo(x, y);
  await page.mouse.down();
  await page.waitForTimeout(80);
  await page.mouse.up();
  await page.waitForTimeout(250);
}
async function click(locator) {
  const box = await locator.boundingBox();
  if (!box) throw new Error(`not visible: ${locator}`);
  await clickAt(box.x + box.width / 2, box.y + box.height / 2);
}

await page.goto(MAP);
const live = page.locator(".list li[data-drone-id]:not(.lost)");
await live.nth(9).waitFor({ timeout: 30_000 });
await page.waitForTimeout(1500); // tiles
const start = (Date.now() - recording) / 1000;
await page.mouse.move(mouse.x, mouse.y);
await page.waitForTimeout(2500); // the fleet flying

// One drone: log in, hold it in GUIDED, plan a mission around it, fly it.
await click(live.nth(Number(process.env.DEMO_DRONE ?? 5)));
await page.waitForTimeout(900); // the map centres on it
await click(page.getByLabel("Password"));
await page.getByLabel("Password").pressSequentially("murmur", { delay: 70 });
await click(page.getByRole("button", { name: "Log in" }));
await page.getByRole("button", { name: "Set mode" }).waitFor();
await page.waitForTimeout(400);
await click(page.getByRole("button", { name: "Set mode" })); // GUIDED, the default: it stops and hovers
await page.getByRole("list", { name: "Commands" }).getByText("Accepted").first().waitFor({ timeout: 10_000 });

// Zoom in on it: at the fleet's zoom a 10 m/s drone hardly moves.
const map = await page.locator(".map").boundingBox();
const droneAt = await page.locator(".drone.selected .arrow").boundingBox();
await moveTo(droneAt.x + droneAt.width / 2, droneAt.y + droneAt.height / 2);
for (let i = 0; i < 6; i++) {
  await page.mouse.wheel(0, -240);
  await page.waitForTimeout(220);
}
await page.waitForTimeout(800);

// Drag the drone into the middle of the map the panel leaves free.
const mid = { x: map.x + (map.width - 340) / 2, y: map.y + map.height / 2 };
let at = await page.locator(".drone.selected .arrow").boundingBox();
const grab = { x: at.x + at.width / 2 - 70, y: at.y + at.height / 2 + 60 }; // beside it, not on it
await moveTo(grab.x, grab.y);
await page.mouse.down();
await page.mouse.move(grab.x + mid.x - (at.x + at.width / 2), grab.y + mid.y - (at.y + at.height / 2), { steps: 25 });
await page.mouse.up();
await page.waitForTimeout(600);

await click(page.getByRole("button", { name: "Plan on map" }));
at = await page.locator(".drone.selected .arrow").boundingBox();
const cx = at.x + at.width / 2;
const cy = at.y + at.height / 2;
for (const [x, y] of [[0.5, -1.1], [1.4, -0.1], [0.4, 1.1], [-1.2, 0.4]]) {
  await clickAt(cx + x * 120, cy + y * 110);
}
await click(page.getByRole("button", { name: "Upload" }));
await page.getByText(/4 waypoints at 20 m: uploaded/).waitFor({ timeout: 15_000 });
await page.waitForTimeout(700);
await click(page.getByRole("button", { name: "Start mission" }));
await page.waitForTimeout(300);
await click(page.getByRole("button", { name: "Confirm" }));
await page.waitForTimeout(9000); // it flies the route
await page.screenshot({ path: join(OUT, "map.png") });

// Out again, and the whole fleet home.
await moveTo(map.x + (map.width - 340) / 2, map.y + map.height / 2);
for (let i = 0; i < 7; i++) {
  await page.mouse.wheel(0, 240);
  await page.waitForTimeout(220);
}
await page.waitForTimeout(800);
await click(page.getByRole("button", { name: "Return all to launch" }));
await page.waitForTimeout(300);
await click(page.getByRole("button", { name: "Confirm" }));
await page.getByTestId("fleet-summary").and(page.locator('[data-waiting="0"]')).waitFor({ timeout: 15_000 });
await page.waitForTimeout(3000);
const end = (Date.now() - recording) / 1000;

const video = page.video();
await context.close(); // finishes the video
const webm = await video.path();

// Two passes: a palette made for this clip, then the GIF with it.
const gif = join(OUT, "demo.gif");
execFileSync("ffmpeg", [
  "-y", "-loglevel", "error", "-ss", String(start), "-t", String(end - start), "-i", webm,
  "-vf", "fps=10,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];" +
    "[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle",
  `${gif}.tmp.gif`,
]);
renameSync(`${gif}.tmp.gif`, gif);
rmSync(videoDir, { recursive: true, force: true });

// Grafana's pipeline dashboard, without its menus.
const grafana = await browser.newPage({ viewport: { width: 1440, height: 1180 } });
await grafana.goto(`${GRAFANA}/d/murmur-pipeline?orgId=1&from=now-${process.env.GRAFANA_RANGE ?? "5m"}&to=now&kiosk`);
await grafana.waitForTimeout(6000); // panels query and draw
await grafana.screenshot({ path: join(OUT, "grafana.png") });

await browser.close();
console.log(`wrote demo.gif (${(end - start).toFixed(0)} s), map.png and grafana.png in ${OUT}`);
