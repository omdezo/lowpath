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
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const APP = path.join(ROOT, "bin", "lowpath.exe");
const LPCHECK = path.join(ROOT, "dist", "lpcheck.exe");
const OUT = path.join(ROOT, "e2e", "out");
// LOWPATH_E2E_PORT lets two worktrees run the test at the same time.
const PORT = Number(process.env.LOWPATH_E2E_PORT) || 7891;
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

// speedServer stands in for Cloudflare's speed-test endpoints, so the
// bufferbloat test runs offline and without using the user's data plan:
// GET /__down?bytes=N sends N bytes, POST /__up swallows the body.
// Downloads are paced (16 KiB per timer tick per stream, a few hundred
// Mbit/s) so loopback can't exhaust the test's data cap in the ramp-up.
function speedServer() {
  const chunk = Buffer.alloc(16 << 10, 7);
  const srv = http.createServer((req, res) => {
    const url = new URL(req.url, "http://x");
    if (req.method === "GET" && url.pathname === "/__down") {
      let left = Math.min(Number(url.searchParams.get("bytes")) || 0, 50e6);
      res.writeHead(200, { "Content-Length": left });
      const pump = () => {
        if (left <= 0) return res.end();
        const n = Math.min(left, chunk.length);
        left -= n;
        const ok = res.write(n === chunk.length ? chunk : chunk.subarray(0, n));
        if (ok) setTimeout(pump, 1);
        else res.once("drain", () => setTimeout(pump, 1));
      };
      res.on("close", () => (left = 0));
      return pump();
    }
    if (req.method === "POST" && url.pathname === "/__up") {
      // Read slowly too: TCP backpressure paces the upload.
      req.on("data", () => {
        req.pause();
        setTimeout(() => req.resume(), 8);
      });
      req.on("end", () => res.end("ok"));
      return;
    }
    res.writeHead(404).end();
  });
  return new Promise((resolve) => srv.listen(0, "127.0.0.1", () => resolve(srv)));
}

// inventory is the file the app reads the "other network software" from
// (-compat-inventory): the test writes "ExitLag is running" into it, and the
// PC's real boosters and VPNs never change the results.
function startApp(data, importFile, bloatServer, inventory) {
  const log = fs.createWriteStream(path.join(OUT, "lowpath.log"));
  const app = spawn(
    APP,
    ["-headless", "-no-elevate", "-addr", `127.0.0.1:${PORT}`, "-data", data, "-relays", importFile, "-bloat-server", bloatServer, "-trace-count", "15", "-trace-interval", "200ms", "-compat-inventory", inventory],
    { windowsHide: true },
  );
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
// scrollToStart brings the top of the first element matching sel to the top of its scroller.
const scrollToStart = (page, sel) => page.locator(sel).first().evaluate((el) => el.scrollIntoView({ block: "start" })).catch(() => {});

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

// waitForServers waits for a Valve game's POP list to download and be pinged.
async function waitForServers(page, id, ms = 30000) {
  const end = Date.now() + ms;
  let l = null;
  while (Date.now() < end) {
    l = (await api(page, "GET", `/games/${id}/servers`)).body;
    if (l && l.state !== "loading" && !l.pinging && l.pinged_at) return l;
    if (l?.state === "error") return l;
    await page.waitForTimeout(500);
  }
  return l;
}

// waitForCurrent waits for the current server card's data to settle: a
// server with its network looked up and a ping verdict.
async function waitForCurrent(page, id, ms = 5000) {
  const end = Date.now() + ms;
  let c = null;
  while (Date.now() < end) {
    c = (await api(page, "GET", `/games/${id}/current-server`)).body;
    const s = c?.server;
    if (s && !s.looking_up && (s.received > 0 || (s.ping_note && s.sent >= 3))) return c;
    await page.waitForTimeout(400);
  }
  return c;
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

  // A previous run that was killed mid-boost (taskkill /F, a crash) leaves
  // its session marker behind. Plant one whose process is gone (a child
  // that already exited) so the app's start-up cleanup has to report it.
  const gone = spawnSync(process.execPath, ["-e", ""], { windowsHide: true }).pid;
  fs.writeFileSync(
    path.join(data, "capture-session.json"),
    JSON.stringify({ backend: "divert", pid: gone, exe: "lowpath.exe", game: "cs2", started: new Date(Date.now() - 3600e3).toISOString() }),
  );

  const speed = await speedServer();
  const inventory = path.join(data, "inventory.json");
  const { app, link } = await startApp(data, importFile, `http://127.0.0.1:${speed.address().port}`, inventory);
  const browser = await chromium.launch({ channel: "msedge", headless: true });
  const page = await (await browser.newContext({ viewport: { width: 1100, height: 720 } })).newPage();
  let helper;
  try {
    // 1. Open the app: no account, straight to the game library.
    await page.goto(link);
    await page.getByLabel("Search games").waitFor();
    await page.getByRole("heading", { name: "Your games" }).waitFor();
    check("App opens straight to the home screen", true);

    // 1b. Fail-open: the leftover marker is cleaned up and explained.
    const notice = page.getByRole("alert", { name: "Boost ended" });
    await notice.waitFor({ timeout: 5000 }).catch(() => {});
    const noticeText = (await notice.textContent().catch(() => "")) ?? "";
    check(
      "A boost the last run never finished is reported at start",
      /Your last boost was cut short/.test(noticeText) && noticeText.includes("Counter-Strike 2") && noticeText.includes("normal connection"),
      noticeText.trim().slice(0, 140),
    );
    check("The leftover session marker is removed", !fs.existsSync(path.join(data, "capture-session.json")));
    await shot(page, "failsafe-recovered", "Start-up cleanup: the last run closed mid-boost; the game went back to the normal connection");
    await notice.getByRole("button", { name: "Dismiss" }).click().catch(() => {});
    check("The notice can be dismissed", !(await notice.isVisible().catch(() => false)));
    // The service forgets it too, so a reload or another window doesn't
    // bring it back (the click's request may still be in flight).
    let after = await live(page);
    for (let i = 0; i < 20 && after?.failsafe; i++) {
      await new Promise((r) => setTimeout(r, 100));
      after = await live(page);
    }
    check("The service forgets a dismissed notice", after != null && !after.failsafe, JSON.stringify(after?.failsafe ?? null).slice(0, 140));
    await shot(page, "home", "Home: pick a game, press BOOST, play");

    // 2. No relays yet: the game screen offers the two ways to start.
    await selectGame(page, "Counter-Strike 2");
    const onboard = page.getByRole("heading", { name: "One quick step before you boost" });
    check("Without relays the game shows the setup choices", await onboard.isVisible().catch(() => false));
    await shot(page, "setup", "No relay yet: try it on this PC, or add your server");

    // 2b. Valve games know their servers before launch: Steam's SDR POP list
    //     with a direct ping from this PC, no relay needed.
    //     The list comes from Steam: offline (no LOWPATH_INTERNET=1) the
    //     card must instead say plainly why there is no list.
    const internet = process.env.LOWPATH_INTERNET === "1";
    const sdr = await waitForServers(page, "cs2");
    const measured = (sdr?.pops || []).filter((p) => p.ping_ms != null);
    const unmeasured = (sdr?.pops || []).filter((p) => p.ping_ms == null);
    const popList = page.getByRole("list", { name: "Valve servers" });
    if (sdr?.state === "ready" || internet) {
      check(
        "CS2 lists Valve's servers with a direct ping",
        sdr?.state === "ready" && (measured.length > 0 || (!internet && unmeasured.every((p) => p.note))),
        sdr?.state === "ready" ? `${sdr.pops.length} POPs, ${measured.length} answered; nearest ${measured.slice(0, 3).map((p) => `${p.id} ${p.ping_ms.toFixed(0)} ms`).join(", ")}` : `${sdr?.state}: ${sdr?.message}`,
      );
      check("POPs without a reply show a reason, not a number", unmeasured.every((p) => p.note), `${unmeasured.length} without a reply`);
      await popList.waitFor({ timeout: 5000 }).catch(() => {});
      const uiRows = await popList.getByRole("listitem").count().catch(() => 0);
      check("Game page shows the Valve server list", uiRows > 0, `${uiRows} rows shown`);
    } else {
      const card = page.getByRole("region", { name: "Valve servers" });
      const said = await card.getByRole("status").first().textContent({ timeout: 15000 }).catch(() => "");
      check("Offline, the Valve servers card says why there is no list", sdr?.state === "error" && !!sdr.message && /Valve's server list/.test(said || ""), `${sdr?.state}: ${said}`);
    }
    await page.getByRole("region", { name: "Valve servers" }).scrollIntoViewIfNeeded().catch(() => {});
    await shot(page, "cs2-servers", "CS2 before launch: Valve's relay POPs with a direct ping from this PC");
    await scrollTop(page);

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

    // 4. Network: your connection (public IP via STUN, ISP via RIPEstat)
    // and the capture mode.
    await page.getByRole("tab", { name: "Network" }).click();
    const conn = page.getByTestId("connection");
    await page
      // A slow registry can take a 15 s lookup plus one automatic retry.
      .waitForFunction(() => document.querySelector('[data-testid="connection"]')?.getAttribute("aria-busy") === "false", null, { timeout: 40000 })
      .catch(() => {});
    const where = (await api(page, "GET", "/location")).body;
    const v4 = where?.ipv4;
    const headline = (await conn.locator(".conn-headline").textContent().catch(() => "")) ?? "";
    const connText = (await conn.textContent().catch(() => "")) ?? "";
    check(
      "Your connection shows the public IP and ISP",
      where?.state === "ok" && v4?.asn > 0 && headline.includes(`AS${v4.asn}`) && connText.includes(v4.ip),
      v4 ? `${headline}; ${v4.ip} via ${v4.source}` : `${where?.state}: ${where?.message ?? "no answer"}`,
    );
    check("STUN and lpcheck see the same public IP", !base.public || !v4 || base.public.startsWith(v4.ip + ":"), `${v4?.ip} vs ${base.public}`);
    const other = page.getByTestId("other-software");
    await other.locator(".conn-headline", { hasText: "None found" }).waitFor({ timeout: 5000 }).catch(() => {});
    check("Other network software: none on a clean system", ((await other.locator(".conn-headline").textContent().catch(() => "")) ?? "") === "None found");
    await shot(page, "network", `Settings → Network: your connection and capture mode (${admin ? "admin" : "no admin: route mode warns"})`);
    await page.getByRole("radio", { name: /Proxy mode/ }).click();
    await page.waitForTimeout(500);

    // 4b. Diagnostics: the bufferbloat test, against the local load server.
    await page.getByRole("tab", { name: "Diagnostics" }).click();
    await page.getByRole("button", { name: "Run test" }).waitFor();
    await shot(page, "diagnostics", "Settings → Diagnostics: the verdict with nothing measured yet");

    // The router pinger runs while the tab is open (20 Hz), and its card
    // shows numbers, or "—" and why (a router that ignores ping, no rights).
    let gw;
    for (let i = 0; i < 40; i++) {
      gw = (await api(page, "GET", "/diag/gateway")).body;
      if (gw && gw.state !== "measuring" && gw.state !== "idle" && (gw.state !== "on" || gw.stats?.sent >= 60)) break;
      await page.waitForTimeout(250);
    }
    const gs = gw?.stats;
    check(
      "Router pinger runs while Diagnostics is open",
      gw?.running && (gw.state === "on" ? gs?.sent >= 60 && gs.p50_ms != null && gw.rate_hz >= 2 && gw.series?.length > 0 : !!gw?.message),
      gw
        ? gw.state === "on"
          ? `${gw.rate_hz} Hz on ${gw.link || "unknown link"}: ${gs.sent} sent, ${gs.lost} lost, median ${gs.p50_ms?.toFixed?.(1)} ms, p95 ${gs.p95_ms?.toFixed?.(1)} ms, ${gs.spikes} spikes`
          : `${gw.state}: ${gw.message}`
        : "no answer",
    );
    const gwCard = page.locator('section[aria-label="Router pings"]');
    const gwShown = gw?.state === "on" ? await gwCard.locator(".gw-stats .stat").count() : await gwCard.locator(".gw-msg").count();
    check("Router pings card shows the numbers or the reason", gwShown > 0, `${gwShown} ${gw?.state === "on" ? "stats" : "message"}`);
    // The verdict refreshes every 4 s; then Home carries the router pings.
    if (gw?.state === "on") await page.locator('.seg[data-segment="home"]:not([data-status="unknown"])').waitFor({ timeout: 6000 }).catch(() => {});
    const segs = await page.locator(".segs .seg").evaluateAll((els) => els.map((e) => `${e.dataset.segment}:${e.dataset.status}`));
    check(
      "Route bar: Home, ISP, Lowpath relay and Game server; Home from the router pings",
      segs.length === 4 && segs[0].startsWith("home:") && segs[3].startsWith("server:") && (gw?.state !== "on" || !segs[0].endsWith(":unknown")),
      segs.join(" "),
    );
    await scrollToStart(page, ".segs");
    await shot(page, "route-segments", "Diagnostics: the route part by part, Home from the live router pings");
    await scrollToStart(page, 'section[aria-label="Router pings"]');
    await shot(page, "router-pings", `Router pings: ${gw?.state === "on" ? `${gw.rate_hz} a second, the last minute charted` : gw?.state ?? "—"}`);
    await scrollToStart(page, 'section[aria-label="Bufferbloat test"]');
    await page.getByRole("button", { name: "Run test" }).click();
    await page.locator(".bloat-live", { hasText: "Downloading" }).waitFor({ timeout: 15000 }).catch(() => {});
    const gwBloat = (await api(page, "GET", "/diag/gateway")).body;
    check("Router pinger pauses while the bufferbloat test loads the line", gwBloat?.state === "paused" && !gwBloat.running, gwBloat?.state);
    await page.waitForTimeout(1500);
    await scrollToStart(page, 'section[aria-label="Bufferbloat test"]');
    await shot(page, "bloat-running", "Bufferbloat test running: download phase, live ping and load");
    let bloat;
    for (let i = 0; i < 80; i++) {
      bloat = (await api(page, "GET", "/diag/bloat")).body;
      if (!bloat?.running) break;
      await page.waitForTimeout(500);
    }
    const br = bloat?.result;
    check(
      "Bufferbloat test finishes with a result",
      bloat && !bloat.running && br && br.phases?.length === 4,
      br ? `grade ${br.grade || "none"}, idle ${br.idle_ms?.toFixed?.(1)} ms, +${br.down_increase_ms?.toFixed?.(1)} / +${br.up_increase_ms?.toFixed?.(1)} ms, ${Math.round(br.data_mb)} MB; to ${br.target}${br.note ? `; ${br.note}` : ""}` : JSON.stringify(bloat),
    );
    check("Bufferbloat test loaded the line both ways", br && br.phases?.[1]?.load_mbps > 0 && br.phases?.[2]?.load_mbps > 0, br && `${br.phases?.[1]?.load_mbps?.toFixed?.(0)} / ${br.phases?.[2]?.load_mbps?.toFixed?.(0)} Mbit/s`);
    check("A grade comes with a measured idle ping", br && (!br.grade || br.idle_ms > 0), br && `grade ${br.grade || "none"}`);
    await page.getByRole("button", { name: "Run again" }).waitFor({ timeout: 5000 }).catch(() => {});
    await scrollToStart(page, 'section[aria-label="Bufferbloat test"]');
    await shot(page, "bloat-result", `Bufferbloat result: grade ${br?.grade || "none (see note)"}`);

    // 4c. Diagnostics: route trace (MTR), shortened to 15 rounds by the
    // -trace-* flags. First UDP to the loopback relay's port, then ICMP to
    // Cloudflare through the real routers on the way.
    const traceDone = async () => {
      let tr;
      for (let i = 0; i < 60; i++) {
        tr = (await api(page, "GET", "/diag/trace")).body;
        if (tr && !tr.running) break;
        await page.waitForTimeout(500);
      }
      return tr;
    };
    await page.locator(".trace").scrollIntoViewIfNeeded();
    await page.getByLabel("Trace target").selectOption("relay:loopback");
    await page.getByRole("radio", { name: "UDP", exact: true }).click();
    await page.getByRole("button", { name: "Start trace" }).click();
    await page.locator(".trace-progress").waitFor({ timeout: 5000 }).catch(() => {});
    await page.waitForTimeout(1200);
    await page.locator(".trace").scrollIntoViewIfNeeded();
    await shot(page, "trace-running", "Route trace running: UDP to the loopback relay's port");
    let tr = await traceDone();
    let rt = tr?.result;
    check(
      "Route trace over UDP reaches the loopback relay",
      tr && !tr.running && rt?.mode === "udp" && rt.reached && rt.end?.recv > 0,
      rt ? `${rt.hops?.length ?? 0} hops, UDP ${rt.end?.recv ?? 0}/${rt.end?.sent ?? 0}: ${rt.verdict}` : JSON.stringify(tr),
    );
    await page.getByRole("button", { name: "Trace again" }).waitFor({ timeout: 5000 }).catch(() => {});
    await page.locator(".trace").scrollIntoViewIfNeeded();
    await shot(page, "trace-udp", "Route trace result: UDP to the loopback relay");
    await page.getByLabel("Trace target").selectOption("cloudflare");
    await page.getByRole("button", { name: "Trace again" }).click();
    tr = await traceDone();
    rt = tr?.result;
    check(
      "Route trace over ICMP finds the routers to Cloudflare",
      rt?.mode === "icmp" && rt.target === "1.1.1.1" && ((rt.reached && rt.hops?.length >= 2) || rt.verdict),
      rt ? `${rt.hops?.length ?? 0} hops, reached ${rt.reached}, destination loss ${rt.dest_loss_pct ?? "—"}%: ${rt.verdict}` : JSON.stringify(tr),
    );
    const report = await page.evaluate(async () => (await fetch("/api/diag/trace/report")).text());
    check("Route trace report exports as text", report.startsWith("Lowpath route trace to Cloudflare DNS (1.1.1.1)") && report.includes("Persist%"), `${report.split("\n").length} lines`);
    await page.getByRole("button", { name: "Trace again" }).waitFor({ timeout: 5000 }).catch(() => {});
    await page.locator(".trace-foot").scrollIntoViewIfNeeded();
    await shot(page, "trace-icmp", `Route trace result: ICMP to Cloudflare, ${rt?.hops?.length ?? 0} hops`);

    // 4d. The verdict reads the tests above (it refreshes every 4 s).
    await page.waitForTimeout(4500);
    await scrollToStart(page, ".verdict");
    const vd = (await api(page, "GET", "/diag/verdict")).body;
    const vchk = (rule) => vd?.checks?.find((c) => c.rule === rule);
    const shown = (await page.locator(".verdict-main .t").textContent().catch(() => "")) ?? "";
    check(
      "Verdict reads the bufferbloat test and the route trace",
      vd?.checks?.length === 5 && vchk("home")?.status !== "unknown" && vchk("isp")?.summary && shown === vd.verdict.title,
      vd ? `${vd.verdict.id} (${vd.verdict.confidence}): ${vd.verdict.title}; ${vd.checks.map((c) => `${c.rule} ${c.status}`).join(", ")}` : "no answer",
    );
    const diagText = await page.evaluate(async () => (await fetch("/api/diag/verdict/report")).text());
    // Shared reports keep only the network part of public addresses.
    const privateV4 = (a) => /^(10\.|127\.|192\.168\.|169\.254\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.)/.test(a);
    const fullAddrs = (diagText.match(/\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b/g) ?? []).filter((a) => !a.endsWith(".0") && !privateV4(a));
    check(
      "Diagnosis report exports as text without full public addresses",
      diagText.startsWith("Lowpath diagnosis") && diagText.includes("VERDICT: ") && diagText.includes("CHECKS") && fullAddrs.length === 0,
      `${diagText.split("\n").length} lines${fullAddrs.length ? `; full addresses: ${fullAddrs.length}` : ""}`,
    );
    await page.getByRole("button", { name: /Show the measurements/ }).click().catch(() => {});
    await scrollToStart(page, ".verdict");
    await shot(page, "verdict", `Diagnostics verdict with the measurements open: ${vd?.verdict?.title ?? "—"}`);
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
    const cur = await waitForCurrent(page, GAME_ID);
    const cs = cur?.server;
    check(
      "Current server card names the game's server during the boost",
      cur?.state === "live" && cur.source === "boost" && cs?.addr.startsWith(stunIP) && (cs.network?.asn > 0 || !!cs.network_note) && (cs.ping_ms != null || !!cs.ping_note),
      cs && `${cs.addr} ${cs.network?.asn ? `AS${cs.network.asn} ${cs.network.isp || ""}` : cs.network_note}, ping ${cs.ping_ms != null ? cs.ping_ms.toFixed(1) + " ms" : cs.ping_note}, geo ${cs.geo?.state}`,
    );
    await page.getByRole("region", { name: "Current server" }).scrollIntoViewIfNeeded().catch(() => {});
    await shot(page, "current-server", "Current server: the game's server, its network, a ping-checked location and live ping");
    await scrollTop(page);
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

    // 7b. Another booster: with ExitLag running, Boost names it instead of
    // starting a capture that fights ExitLag's driver.
    const setInventory = async (inv) => {
      if (inv) fs.writeFileSync(inventory, JSON.stringify(inv));
      else fs.rmSync(inventory, { force: true });
      await api(page, "GET", "/conflicts?fresh=1");
      await page.waitForTimeout(1200); // the 2 Hz live feed carries it to the banner
    };
    await setInventory({
      processes: ["ExitLag.exe"],
      programs: ["ExitLag"],
      services: [{ name: "ndisrd", display: "WinpkFilter LWF", driver: true, running: true }],
    });
    const cres = await api(page, "POST", "/apply", { game_id: GAME_ID, mode: "smart" });
    check(
      "Boost next to a running ExitLag is refused and names it",
      cres.status === 409 && cres.body?.error === "conflict" && /^ExitLag is running/.test(cres.body?.message) && cres.body?.conflicts?.[0]?.id === "exitlag",
      `${cres.status} ${cres.body?.error}: ${cres.body?.message}`,
    );
    await boostBtn(page, "lpcheck").click();
    const refusal = page.locator(".boost-refused");
    await refusal.waitFor({ timeout: 5000 }).catch(() => {});
    check("The Boost button says to close ExitLag first", /ExitLag is running.*close it first/.test((await refusal.textContent().catch(() => "")) ?? ""));
    check("…and offers Boost anyway", await refusal.getByRole("button", { name: "Boost anyway" }).isVisible().catch(() => false));
    const cbanner = page.getByRole("alert", { name: "Other network software" });
    check("The banner names ExitLag and the leftover driver", (await cbanner.textContent().catch(() => ""))?.includes("ExitLag is running.") && (await cbanner.textContent())?.includes("Windows Packet Filter is loaded."));
    await cbanner.getByRole("button", { name: "Why?" }).first().click();
    check("Why? explains the conflict", await cbanner.getByText(/Two programs rerouting the same packets/).isVisible().catch(() => false));
    await scrollTop(page);
    await shot(page, "conflict-exitlag", "ExitLag running: the banner names it and says what to do, with a one-click explanation");
    await refusal.scrollIntoViewIfNeeded().catch(() => {});
    await shot(page, "conflict-boost", "ExitLag running: Boost says to close it first, or Boost anyway");
    if (!admin) {
      const anyway = await api(page, "POST", "/apply", { game_id: GAME_ID, mode: "smart", ignore_conflicts: true });
      check("Boost anyway skips the conflict check", anyway.body?.error === "needs_admin", `${anyway.status} ${anyway.body?.error}`);
    }
    await openSettings(page, "Network");
    await page.getByTestId("other-software").getByRole("button", { name: "Check again" }).click();
    await page.getByTestId("other-software").locator(".conn-headline", { hasText: "2 found" }).waitFor({ timeout: 5000 }).catch(() => {});
    await page.getByTestId("other-software").scrollIntoViewIfNeeded();
    check("Settings lists the other network software", ((await page.getByTestId("other-software").textContent()) ?? "").includes("ExitLag is running."));
    await shot(page, "settings-other-software", "Settings → Network: other network software found, with what to do about each");
    await closeSettings(page);
    // Closed but still installed: named quietly, Boost is free again.
    await setInventory({ programs: ["ExitLag"] });
    const idle = page.getByLabel("Installed network software");
    check("An installed ExitLag is named", ((await idle.textContent().catch(() => "")) ?? "").includes("ExitLag is installed."));
    check("The refusal goes once ExitLag is closed", !(await refusal.isVisible().catch(() => false)));
    await scrollTop(page);
    await shot(page, "conflict-installed", "ExitLag installed but closed: a quiet note, Boost works");
    await setInventory(null);
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
    speed.close();
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
