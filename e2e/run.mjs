// End-to-end test: runs the real Lowpath app (bin\lowpath.exe), drives its UI
// in Microsoft Edge the way a user would, pushes real internet traffic through
// it with lpcheck (STUN to Google), checks the numbers, and saves a screenshot
// of every step plus a report to e2e\out\.
//
//   cd e2e && npm install && node run.mjs
//
// From a normal terminal it tests proxy mode end to end and checks that route
// mode asks for admin rights. From an administrator terminal it also tests
// route mode: real WinDivert capture of lpcheck.exe, tunnel, and injection.
import { chromium } from "playwright-core";
import { spawn, spawnSync } from "node:child_process";
import { lookup } from "node:dns/promises";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const APP = path.join(ROOT, "bin", "lowpath.exe");
const LPCHECK = path.join(ROOT, "dist", "lpcheck.exe");
const OUT = path.join(ROOT, "e2e", "out");
const PORT = 7891;
const STUN = { host: "stun.l.google.com", port: 19302 };
const GAME_ID = "custom-lpcheck";

const results = [];
const shots = [];
const lpRuns = [];

function check(step, ok, detail = "") {
  results.push({ step, ok: !!ok, detail });
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`);
  return !!ok;
}

async function shot(page, name, caption) {
  const file = `${String(shots.length + 1).padStart(2, "0")}-${name}.png`;
  // Let artwork finish loading, then transitions and the 2 Hz live feed settle.
  await page.waitForFunction(() => [...document.images].every((i) => i.complete || i.getBoundingClientRect().top > innerHeight), null, { timeout: 8000 }).catch(() => {});
  await page.waitForTimeout(500);
  await page.screenshot({ path: path.join(OUT, file) });
  shots.push({ file, caption });
  console.log(`      screenshot ${file}`);
}

const isAdmin = () => spawnSync("net", ["session"], { stdio: "ignore" }).status === 0;

// lpcheck is the stand-in game: 20 STUN requests/s, prints loss, RTT and the
// public address the STUN server saw.
function lpcheck(label, args) {
  return new Promise((resolve) => {
    const p = spawn(LPCHECK, args, { windowsHide: true });
    let out = "";
    p.stdout.on("data", (d) => (out += d));
    p.stderr.on("data", (d) => (out += d));
    p.on("close", (code) => {
      const m = (re) => out.match(re);
      const tot = m(/sent (\d+), received (\d+), lost (\d+) \(([\d.]+)%\)/);
      const rtt = m(/rtt min\/avg\/max = ([\d.]+)\/([\d.]+)\/([\d.]+) ms/);
      const pub = m(/public address: (\S+)/);
      const local = m(/local (\S+)/);
      const r = {
        label,
        code,
        out: out.trim(),
        sent: tot ? +tot[1] : 0,
        received: tot ? +tot[2] : 0,
        avg: rtt ? +rtt[2] : NaN,
        public: pub ? pub[1] : "",
        local: local ? local[1] : "",
      };
      lpRuns.push(r);
      resolve(r);
    });
  });
}

function startApp(data, importFile) {
  const log = fs.createWriteStream(path.join(OUT, "lowpath.log"));
  const app = spawn(APP, ["-headless", "-no-elevate", "-addr", `127.0.0.1:${PORT}`, "-data", data, "-relays", importFile], { windowsHide: true });
  app.stdout.pipe(log);
  return new Promise((resolve, reject) => {
    let buf = "";
    const timer = setTimeout(() => reject(new Error("Lowpath did not print its launch link within 20 s")), 20000);
    app.stderr.on("data", (d) => {
      const s = d.toString();
      buf += s;
      // The one-time launch link must not end up in the saved log.
      log.write(s.replace(/\?t=\S+/g, "?t=<redacted>"));
      const m = buf.match(/works once\): (http\S+)/);
      if (m) {
        clearTimeout(timer);
        resolve({ app, link: m[1] });
      }
    });
    app.on("exit", (code) => reject(new Error(`Lowpath exited early (code ${code})`)));
  });
}

// api calls the daemon from inside the page, so it carries the session cookie.
const api = (page, method, p, body) =>
  page.evaluate(
    async ([method, p, body]) => {
      const r = await fetch("/api" + p, {
        method,
        headers: { "X-Lowpath": "1", ...(body ? { "Content-Type": "application/json" } : {}) },
        body: body ? JSON.stringify(body) : undefined,
      });
      return { status: r.status, body: await r.json().catch(() => null) };
    },
    [method, p, body],
  );

const live = async (page) => (await api(page, "GET", "/bootstrap")).body.live;

async function openSettings(page, tab) {
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("tab", { name: tab }).click();
}

// scrollPanel scrolls the scrollable container around the element showing
// text all the way down, so content below the fold (graph, servers) is shown.
const scrollPanel = (page, text) =>
  page.evaluate((text) => {
    const el = [...document.querySelectorAll("*")].find((e) => e.childElementCount === 0 && e.textContent.includes(text));
    for (let p = el?.parentElement; p; p = p.parentElement) {
      if (p.scrollHeight > p.clientHeight + 8 && /(auto|scroll)/.test(getComputedStyle(p).overflowY)) {
        p.scrollTop = p.scrollHeight;
        return;
      }
    }
    window.scrollTo(0, document.body.scrollHeight);
  }, text);

// scrollTop brings the main panel back to the top (banners live there).
const scrollTop = (page) => page.evaluate(() => document.querySelector(".main")?.scrollTo(0, 0));

const closeSettings = (page) => page.getByRole("button", { name: "Close settings" }).click();

const selectGame = (page, name) => page.locator(".game-row", { hasText: name }).first().click();

const boostBtn = (page, game) => page.getByRole("button", { name: `Boost ${game}` });
const stopBtn = (page, game) => page.getByRole("button", { name: `Stop optimizing ${game}` });

// waitForRoutes waits for the automatic route measurement after a game is picked.
async function waitForRoutes(page, id, ms = 20000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    const r = (await api(page, "GET", `/games/${id}/routes`)).body;
    if (r?.analysis) return r;
    await page.waitForTimeout(400);
  }
  return null;
}

async function openDetails(page) {
  const t = page.getByRole("button", { name: /Details/ });
  if ((await t.getAttribute("aria-expanded")) !== "true") await t.click();
}

async function waitForStop(page, ms) {
  try {
    await boostBtn(page, "lpcheck").waitFor({ timeout: ms });
    return true;
  } catch {
    return false;
  }
}

async function main() {
  fs.rmSync(OUT, { recursive: true, force: true });
  fs.mkdirSync(OUT, { recursive: true });
  for (const f of [APP, LPCHECK]) {
    if (!fs.existsSync(f)) throw new Error(`${f} is missing: run build.ps1 first`);
  }
  const admin = isAdmin();
  console.log(`Lowpath e2e - ${admin ? "administrator: proxy + route mode" : "not administrator: proxy mode (route mode only checks the admin prompt)"}`);

  const stunIP = (await lookup(STUN.host, { family: 4 })).address;
  const data = fs.mkdtempSync(path.join(os.tmpdir(), "lowpath-e2e-"));
  // Proxy mode needs to know the "game server": lpcheck's STUN server.
  const importFile = path.join(data, "relays-import.json");
  fs.writeFileSync(importFile, JSON.stringify({ games: { [GAME_ID]: { server: `${stunIP}:${STUN.port}` } } }));

  // Baseline: lpcheck straight to Google, no Lowpath involved.
  const base = await lpcheck("direct (no Lowpath)", ["-for", "3s"]);
  check("Baseline: lpcheck reaches Google STUN directly", base.received > 0, `${base.received}/${base.sent} replies, ${base.avg} ms, public ${base.public}`);

  const { app, link } = await startApp(data, importFile);
  const browser = await chromium.launch({ channel: "msedge", headless: true });
  const page = await (await browser.newContext({ viewport: { width: 1100, height: 720 } })).newPage();
  let helper;
  try {
    // 1. Open the app: no account, straight to the game library.
    await page.goto(link);
    await page.getByLabel("Search games").waitFor();
    await page.getByRole("heading", { name: "Your games" }).waitFor();
    check("App opens straight to the home screen", true);
    await shot(page, "home", "Home: pick a game, press BOOST, play");

    // 2. No relays yet: the game screen offers the two ways to start.
    await selectGame(page, "Counter-Strike 2");
    const onboard = page.getByRole("heading", { name: "One quick step before you boost" });
    check("Without relays the game shows the setup choices", await onboard.isVisible().catch(() => false));
    await shot(page, "setup", "No relay yet: try it on this PC, or add your server");

    // 3. Relays: turn on the loopback test relay and test it.
    await page.getByRole("button", { name: /Add my server/ }).click();
    await page.getByRole("switch", { name: "Use loopback test relay" }).check();
    const relays = (await api(page, "GET", "/relays")).body.relays;
    const loop = relays.find((r) => r.builtin);
    check("Loopback test relay is listed", !!loop, loop ? `${loop.label} ${loop.addr}` : "missing");
    await page.getByRole("button", { name: /^Test / }).first().click();
    await page.waitForTimeout(3000);
    const t = loop && (await api(page, "POST", "/relays/test", { id: loop.id })).body;
    check("Relay test gets answers", t && t.ok, t && `${t.rtt_ms?.toFixed?.(2)} ms, ${t.loss_pct}% loss: ${t.message}`);
    await shot(page, "relays", "Settings → Relays: loopback relay on and tested");

    // 4. Network: capture mode.
    await page.getByRole("tab", { name: "Network" }).click();
    await shot(page, "network", `Settings → Network: capture mode (${admin ? "admin" : "no admin: route mode warns"})`);
    await page.getByRole("radio", { name: /Proxy mode/ }).click();
    await page.waitForTimeout(500);
    await closeSettings(page);

    // 5. Add lpcheck.exe as a custom game, picked from the running programs.
    helper = spawn(LPCHECK, ["-for", "60s", "-rate", "2"], { windowsHide: true });
    await page.waitForTimeout(1500);
    await page.getByTitle("Add a game that isn't in the list").click();
    await page.getByRole("radio", { name: /lpcheck\.exe/i }).click();
    await page.locator("#custom-game-name").fill("lpcheck");
    await shot(page, "add-game", "Add game: picking lpcheck.exe from programs with UDP sockets");
    await page.locator(".modal-foot").getByRole("button", { name: "Add game" }).click();
    await page.locator(".game-row", { hasText: "lpcheck" }).first().waitFor();
    helper.kill();
    helper = null;
    check("Custom game lpcheck added", true);

    // 6. Picking the game measures the routes by itself: the relay, and a
    //    direct ping to the game server.
    await selectGame(page, "lpcheck");
    const routes = (await waitForRoutes(page, GAME_ID)) || {};
    await page.waitForTimeout(800);
    const a = routes.analysis || {};
    const pathStats = Object.values(a.paths || {})[0];
    check("Routes measured on their own: direct ping to the server", a.direct_known && a.direct?.rtt_ms > 0, `before ${a.direct?.rtt_ms?.toFixed(1)} ms`);
    check("Routes measured on their own: relay route", pathStats?.up, pathStats && `${pathStats.rtt_ms.toFixed(1)} ms, ${pathStats.loss_pct}% loss`);
    await shot(page, "ready", "Ready: routes measured automatically, one button to press");

    // 7. BOOST in proxy mode and push real traffic through the tunnel.
    await boostBtn(page, "lpcheck").click();
    await stopBtn(page, "lpcheck").waitFor();
    let l = await live(page);
    check("Apply starts optimizing in proxy mode", l.status === "optimizing" && l.snapshot?.listen, `listening on ${l.snapshot?.listen}`);
    await shot(page, "boosting", "Boosting (proxy mode): waiting for the game's packets");

    const run = lpcheck("proxy mode via loopback relay", ["-server", l.snapshot.listen, "-for", "20s"]);
    await page.waitForTimeout(12000);
    l = await live(page);
    await shot(page, "live-traffic", "Live: real traffic through the relay, ping in the button, packets moving on the route map");
    await openDetails(page);
    await scrollPanel(page, "Live ping");
    await shot(page, "live-graph", "Live: 60 s ping graph, direct (before) vs Lowpath (after)");
    await page.getByRole("button", { name: /Details/ }).click(); // fold it away again
    const pr = await run;
    const s = l.snapshot || {};
    check("lpcheck gets its replies through Lowpath", pr.sent > 0 && pr.received / pr.sent >= 0.95, `${pr.received}/${pr.sent} replies, ${pr.avg} ms, server saw ${pr.public}`);
    check("Engine counted packets both ways", s.pkts_up > 0 && s.pkts_down > 0, `up ${s.pkts_up}, down ${s.pkts_down}, duplicates dropped ${s.dups_dropped}`);
    check("Before (direct ping) is measured", s.before_known && s.before?.rtt_ms > 0, `${s.before?.rtt_ms?.toFixed(1)} ms`);
    check("After (relay + server leg) is measured", s.after?.up && s.server_leg_known, `${s.after?.rtt_ms?.toFixed(1)} ms incl. ${s.server_leg_ms?.toFixed(1)} ms relay→server`);
    check("Game server detected", (s.servers || []).some((x) => x.addr.startsWith(stunIP)), (s.servers || []).map((x) => x.addr).join(", "));

    // With "Stop on game exit" on (default) the session ends after lpcheck does.
    const autoStopped = await waitForStop(page, 15000);
    if (!autoStopped) await stopBtn(page, "lpcheck").click();
    check("Session ends when the game exits", autoStopped, autoStopped ? "auto-stopped" : "had to press Stop");
    await waitForStop(page, 5000);
    await scrollTop(page);
    await shot(page, "stopped", "Stopped: traffic back to normal");

    // 8. Route mode: transparent capture with WinDivert.
    await openSettings(page, "Network");
    await page.getByRole("radio", { name: /Route mode/ }).click();
    await page.waitForTimeout(500);
    await closeSettings(page);
    await selectGame(page, "lpcheck");
    if (!admin) {
      const res = await api(page, "POST", "/apply", { game_id: GAME_ID, mode: "smart" });
      check("Route mode without admin asks for administrator rights", res.status === 409 && res.body?.error === "needs_admin", `${res.status} ${res.body?.error}`);
      await boostBtn(page, "lpcheck").click();
      await page.waitForTimeout(1200);
      check("UI shows the admin banner", await page.getByText("Route mode can't capture game traffic").isVisible());
      const plat = (await api(page, "GET", "/bootstrap")).body?.platform;
      check("Platform layer offers the restart", plat?.can_elevate === true && plat?.capture?.backend === "divert", JSON.stringify(plat));
      check("Banner offers Restart as administrator", await page.getByRole("button", { name: "Restart as administrator" }).isVisible());
      await scrollTop(page);
      await shot(page, "route-needs-admin", "Route mode without admin: banner offers a restart as administrator");
      // What real games look like on the Boost screen: Steam artwork, and
      // a non-Steam game with the icon from its own program.
      for (const [name, label] of [
        ["Counter-Strike 2", "cs2"],
        ["Valorant", "valorant"],
      ]) {
        await selectGame(page, name);
        await waitForRoutes(page, label === "cs2" ? "cs2" : "valorant", 8000);
        for (const b of await page.getByRole("button", { name: "Dismiss" }).all()) await b.click().catch(() => {});
        await page.waitForTimeout(1200);
        await shot(page, `${label}-ready`, `${name}: ready to boost, with the game's own artwork`);
      }
    } else {
      await boostBtn(page, "lpcheck").click();
      await stopBtn(page, "lpcheck").waitFor();
      await shot(page, "route-waiting", "Route mode: waiting for game traffic");
      // lpcheck talks straight to Google; WinDivert must catch it on its own.
      const rrun = lpcheck("route mode (WinDivert capture)", ["-for", "20s"]);
      await page.waitForTimeout(12000);
      l = await live(page);
      await shot(page, "route-capturing", "Route mode: lpcheck captured transparently");
      await openDetails(page);
      await page.getByRole("list", { name: "Game servers" }).scrollIntoViewIfNeeded();
      await shot(page, "route-servers", "Route mode: detected game server and capture counters (captured / injected)");
      await scrollPanel(page, "Live ping");
      await shot(page, "route-graph", "Route mode: live ping graph");
      const rr = await rrun;
      const c = l.capture || {};
      check("Route mode: lpcheck still gets its replies", rr.sent > 0 && rr.received / rr.sent >= 0.95, `${rr.received}/${rr.sent}, ${rr.avg} ms, server saw ${rr.public} (local ${rr.local})`);
      check("Route mode: packets captured and replies injected", c.captured > 0 && c.injected > 0, `captured ${c.captured}, injected ${c.injected}, errors ${c.errors} ${c.last_error || ""}`);
      check("Route mode: the server saw the relay's socket, not lpcheck's", rr.public && rr.local && rr.public.split(":")[1] !== rr.local.split(":")[1], `local ${rr.local} → public ${rr.public}`);
      if (!(await waitForStop(page, 15000))) await stopBtn(page, "lpcheck").click();
      await waitForStop(page, 5000);
      await shot(page, "route-stopped", "Route mode stopped");
      const after = await lpcheck("direct after stopping", ["-for", "3s"]);
      check("Networking normal after Stop", after.received / Math.max(after.sent, 1) >= 0.95, `${after.received}/${after.sent} replies directly`);
    }
  } catch (e) {
    check("Test flow", false, e.message.split("\n")[0]);
    await shot(page, "failure", `Failure: ${e.message.split("\n")[0]}`).catch(() => {});
  } finally {
    helper?.kill();
    await browser.close();
    app.kill();
    writeReport(admin);
  }
  const failed = results.filter((r) => !r.ok).length;
  console.log(`\n${results.length - failed}/${results.length} checks passed. Report: ${path.join(OUT, "report.md")}`);
  process.exitCode = failed ? 1 : 0;
}

function writeReport(admin) {
  const lines = [
    `# Lowpath end-to-end test`,
    ``,
    `Mode: ${admin ? "administrator (proxy + route mode)" : "not administrator (proxy mode; route mode only checks the admin prompt)"}`,
    ``,
    `| Result | Check | Detail |`,
    `|---|---|---|`,
    ...results.map((r) => `| ${r.ok ? "PASS" : "**FAIL**"} | ${r.step} | ${r.detail.replace(/\|/g, "/")} |`),
    ``,
    `## Screenshots`,
    ``,
    ...shots.map((s) => `- \`${s.file}\` - ${s.caption}`),
    ``,
    `## lpcheck runs`,
    ``,
    ...lpRuns.flatMap((r) => [`### ${r.label}`, "```", r.out, "```", ""]),
  ];
  fs.writeFileSync(path.join(OUT, "report.md"), lines.join("\n"));
}

main().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
