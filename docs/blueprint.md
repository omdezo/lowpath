# Game Network Optimizer — Full Product Blueprint ("The Idea File")

> A complete, build-ready specification for a game latency/route optimizer in the
> same category as ExitLag / NoPing / WTFast / Mudfish. This documents **every
> feature, exactly how each one works, how it looks, and the exact end-to-end user
> workflow** so the whole application can be re-created from scratch.
>
> **Scope / legal note:** This is a blueprint for building *your own* competing
> product. It describes the *functional behavior and UX patterns* of this class of
> app (observable product behavior), not decompiled code. When you build it: pick
> your **own name, logo, color palette, copy, icons and server list**. Do **not**
> reuse ExitLag's trademark, brand assets, or proprietary route/server data. The
> engineering ideas below (split-tunnel, multipath, dedup) are general networking
> techniques and are free to implement.
>
> **Lowpath is free and open source (MIT).** Unlike the commercial products in this
> category, it has **no accounts, sign-up, login, trials, plans, subscriptions,
> paywall, billing or device limits**. The app opens straight into the game library
> and Apply/Boost is always available. Where this blueprint describes the category's
> products, anything account- or payment-related does not apply to Lowpath.

---

## 0. TL;DR of the whole product in one paragraph

A desktop app (Windows-first) that, when you pick a game and hit one button,
silently captures *only that game's* network packets, sends them out through the
company's own relay servers over premium backbone paths instead of your ISP's
default route, duplicates each packet across several of those paths so a loss on
one path doesn't cause a hitch, and shows you a live before/after ping, loss and
jitter readout. Around that core it stacks convenience features: a searchable game
library with auto-detect, a traffic prioritizer, OS "FPS boost" tweaks, multi-
connection bonding, and a settings panel, with no accounts or payments. The
entire value proposition is "install → pick game → Apply → play, with a smoother
and often lower ping."

---

## 1. Product concept & positioning

| | |
|---|---|
| **One-liner** | "Optimizes the route between you and the game server, and makes it loss/jitter-proof." |
| **Category** | Selective split-tunnel overlay router for games (NOT a privacy VPN). |
| **Primary platform** | Windows 10/11 (x64). Later: macOS, Android, router firmware. |
| **Business model** | None: free and open source (MIT). No accounts, trials, plans or subscriptions; users bring their own relays. |
| **Core promise** | Lower *and especially more stable* ping; less packet loss; fewer rubber-bands and lag spikes. |
| **Honest boundary** | Cannot beat the speed of light or fix local Wi-Fi; helps most when the ISP's default route is bad. |

**Who it's for:** competitive online gamers (FPS/MOBA/MMO/fighting), players on ISPs
with poor peering, players connecting to servers in another region, players on
unstable connections.

---

## 2. System architecture (the three pieces)

```
┌─────────────────────────────┐        ┌──────────────────────────────┐        ┌───────────────┐
│        DESKTOP CLIENT       │        │      RELAY / EDGE NETWORK    │        │  GAME SERVER  │
│  - UI (Electron/native)     │        │  - Ingress relay (near you)  │        │ (Riot/Valve/  │
│  - Service/daemon (admin)   │ ─────► │  - Transit (premium backbone)│ ─────► │  Blizzard...) │
│  - Packet interceptor (WFP) │ paths  │  - Egress relay (near server)│        └───────────────┘
│  - Tunnel + dedup engine    │ ◄───── │  - Dedup / forward logic     │ ◄─────
│  - Measurement loop         │        └──────────────────────────────┘
└──────────────┬──────────────┘                      ▲
               │                                      │
               ▼                                      │
┌─────────────────────────────┐                       │
│        BACKEND / API        │ ──────────────────────┘ (relay registry, health)
│  - (no accounts / billing)  │
│  - Game catalog (IP ranges) │
│  - Relay registry & health  │
│  - Telemetry / config push  │
└─────────────────────────────┘
```

1. **Desktop client** — UI + privileged background service + kernel packet filter +
   user-space tunnel/dedup engine + continuous path measurement.
2. **Relay network** — geographically distributed servers in well-peered datacenters;
   ingress (close to user), transit (premium links between relays), egress (close to
   the game server). Egress does packet **de-duplication** before handing packets to
   the game server.
3. **Backend/API** (optional, no accounts) — the **game catalog** (each game =
   a set of destination IP CIDRs + ports), the **relay registry** (which relays exist,
   where, current health), config/feature-flag push, telemetry ingestion.

---

## 3. Information architecture (every screen in the app)

```
App
├── Startup (no login: loading → route mapping if there are relays → main window)
├── Main window
│   ├── Left sidebar: Game Library (search, All / Favorites / Installed, A–Z list)
│   ├── Center: Selected-game panel
│   │   ├── Route selector (Recommended / Continent / Specific server list)
│   │   ├── Route cards (each = region/ISP path with live ping/loss/jitter)
│   │   ├── [Analyze] and [Apply Routes] buttons
│   │   └── Live stats strip (before vs after ping, loss %, jitter, graph)
│   ├── Right/overlay: Connection status + per-route health
│   └── Top bar: search, active-game chip, settings gear, minimize-to-tray
├── Settings
│   ├── General (launch on boot, minimize to tray, language, theme)
│   ├── Network (route mode vs proxy mode, adapter select, packet duplication level)
│   ├── Optimization (Traffic Shaper, FPS Boost, Multi-Internet)
│   └── Advanced / Diagnostics (logs, ping test, export report, reset driver)
├── System tray menu (show, quick connect, pause, quit)
└── In-game overlay HUD (optional: live ping/loss/jitter)
```

---

## 4. EXACT end-to-end user workflow

This is the canonical happy-path flow, state by state. Each numbered step =
one thing the user does and what the app shows/does in response.

### 4.1 First run
1. **Download & install.** User gets the installer from the website, runs it. Installer
   requests admin rights (needed to install the network driver/service). A virtual
   network adapter + background service are installed. Reboot optional.
2. **Launch → no login.** There is no account to create or sign in to; the app goes
   straight on.
3. **Route mapping.** If relays are configured, the app shows a brief *"Preparing the
   best routes for you…"* loading state while it runs an initial latency sweep to the
   relays.
4. **Land on Main window** with the Game Library in the left sidebar and an empty-ish
   center panel prompting "Select a game to begin."

### 4.2 Selecting a game
5. **Find the game.** User scrolls the A–Z game list or types in the **search box**.
   Optionally filters to **Favorites** or **Installed** (app can auto-detect installed
   games by scanning known install paths / running processes).
6. **Click the game.** Center panel loads that game's config: its available **route
   options** appear.

### 4.3 Choosing a route
7. **Route options depend on the game:**
   - **Recommended / Smart** (if available for that game): "use all regions worldwide to
     pick the best route for you" — fully automatic. Default and recommended choice.
   - **Continent / Region**: pick a continent or region group; app chooses best relay set
     within it.
   - **Specific server**: a list of individual relay routes (often labeled by region +
     the ISP path, e.g. "US-East via Carrier A"), each shown as a **route card**.
8. **Each route card shows live metrics** once analyzed: *ping (ms), packet loss (%),
   jitter (ms)*, and often a small colored quality indicator (green/yellow/red).

### 4.4 Analyze & apply
9. **Click [Analyze].** The app probes every candidate route to this game's servers and
   fills in the ping/loss/jitter numbers on each route card; it sorts best-first and
   highlights the recommended one.
10. **(Optional) Manually pick** the route with the lowest ping / best stability if the
    user wants control; otherwise leave it on Recommended/auto.
11. **Click [Apply Routes].** This is the activation moment:
    - The service installs the capture rules (WFP filters) for this game's process/IP
      ranges.
    - The tunnel + dedup engine comes up and establishes sessions to the chosen relay(s).
    - Status flips to **Connected / Optimizing** for that game.

### 4.5 Play
12. **Launch the game** (the app advises configuring ExitLag-style routing *first*, then
    opening the game). Game traffic is now captured and routed/duplicated automatically.
13. **Watch live stats.** The stats strip shows **before vs after ping**, current loss and
    jitter, and a rolling graph. Optional **in-game overlay HUD** shows the same numbers
    on top of the game.
14. **Done playing → Stop / Pause** or just close the game; app can auto-detect the game
    closed and tear down the routes, returning traffic to normal.

### 4.6 Ongoing
15. App **continuously re-measures** paths and can **auto-switch** routes if the chosen
    one degrades (feature-flagged). There is no trial countdown or subscription wall:
    Lowpath is free, so optimization never stops for payment reasons.

---

## 5. FEATURE CATALOG — what / how it looks / how it works / how to build

For each feature: **What** (the capability), **Looks** (UI), **Behavior** (UX rules),
**Build** (implementation).

### 5.1 No accounts (free and open source)
- **What:** Lowpath is free and open source (MIT). There are no accounts, sign-up,
  login/logout, trials, plans, subscriptions, paywall, billing or device limits.
- **Looks:** no login or sign-up screens, no trial chip, no Account settings tab, no plan
  wall. The app opens straight into the game library.
- **Behavior:** Apply/Boost is always available. Commercial products in this category gate
  optimization behind a trial and subscription; Lowpath deliberately does not.
- **Build:** nothing to build. The only access control is local: the UI/API answers only
  the app's own window through a per-launch session secret, which keeps other programs on
  the PC out. It is not a login.

### 5.2 Game library
- **What:** searchable catalog of supported games, Favorites, auto-detect installed/running.
- **Looks:** left sidebar list with game icon + name; search box pinned at top; tabs All /
  Favorites / Installed; favorite = star toggle.
- **Behavior:** typing filters instantly; recently used float to top; a running supported
  game gets a "running" badge and can be auto-selected.
- **Build:** catalog served from backend = `{ game_id, name, icon, platforms, server_ranges:
  [CIDR+ports], route_options, smart_available }`. Auto-detect = scan running process names +
  known install dirs/registry. Cache catalog locally, refresh on launch.

### 5.3 Route selection (Recommended / Region / Specific)
- **What:** three tiers of control over which relay path is used.
- **Looks:** a segmented control or dropdown (Recommended | By Region | Specific Server) and
  below it a list of **route cards**. Recommended shows one auto card; Specific shows many.
- **Behavior:** Recommended = app owns the choice and may multipath across several; Region =
  constrained to a continent; Specific = user locks one labeled path. Selected card
  highlighted.
- **Build:** route = ordered relay chain `[ingress → transit* → egress]`. Backend returns
  candidate chains per game per user region. "Smart" = client ranks all and picks top-K for
  duplication.

### 5.4 Route analysis ("Analyze")
- **What:** on-demand measurement of every candidate route to the game's servers.
- **Looks:** an [Analyze] button; while running, route cards show spinners; then fill with
  ping/loss/jitter + a green/yellow/red quality dot; list sorts best-first.
- **Behavior:** can auto-run on game select; re-runnable; results cached briefly.
- **Build:** send probe trains (UDP echo / ICMP) through each chain, measure RTT, loss,
  jitter (stddev of RTT). Also measure the **baseline** (direct, no relay) for before/after.

### 5.5 Apply Routes / Connect (the core activation)
- **What:** turns optimization on for the selected game.
- **Looks:** big primary **[Apply Routes]** button; after click → status pill "Optimizing",
  button becomes "Stop"; per-route health indicators light up.
- **Behavior:** idempotent; applying a new game while one is active either stacks or switches
  (configurable); Stop tears everything down cleanly.
- **Build:** service installs **WFP filters** matching the game process/IP ranges → redirect
  matched flows into the tunnel engine → engine opens relay sessions and begins forwarding +
  duplicating. On Stop, remove filters, close sessions, restore default routing.

### 5.6 Packet interception / split-tunnel (client internals)
- **What:** grab ONLY the game's packets, leave everything else (browser, Discord) alone.
- **Looks:** invisible to user (this is the engine). Surfaced only as "Connected".
- **Build (Windows):** a **WFP callout driver** (or WinDivert in user space for an MVP) that
  matches flows by process image name and/or destination CIDR+port, and redirects them to the
  local tunnel. Alternative: **WinTUN** virtual adapter + route table entries for the game's
  server CIDRs. Non-matching traffic is untouched (true split tunnel).

### 5.7 Multipath routing + packet duplication (the signature feature)
- **What:** send each game packet down several relay paths at once; the egress relay forwards
  the first copy to arrive and drops duplicates. Kills jitter and loss.
- **Looks:** usually automatic; optionally a **"Packet duplication" level** setting
  (Off / Balanced / Max = how many parallel paths, e.g. 1/2/4). Shows as multiple active
  route-health bars.
- **Behavior:** more duplication = smoother but more bandwidth; auto mode scales with measured
  loss. Dedup must be fast and ordered.
- **Build:**
  - Tag each original packet with a monotonically increasing **sequence id** (per flow).
  - Encapsulate and send N copies over N chains.
  - **Egress relay** keeps a sliding window of seen sequence ids per flow; first copy of a
    seq wins → forwarded to game server; later copies dropped. Return path mirrors this.
  - Use UDP transport between relays with your own light header `{flow_id, seq, ts}`.
  - This is conceptually FEC/redundancy for UDP: effective latency ≈ *min* across paths;
    effective loss ≈ *product* of per-path loss.

### 5.8 Live stats (before/after ping, loss, jitter, graph)
- **What:** real-time proof the optimizer is helping.
- **Looks:** a stats strip: "Before 92 ms → After 61 ms", "Loss 0.0%", "Jitter 3 ms", plus a
  rolling line/area **graph** (last 60s). Color-coded.
- **Behavior:** updates ~1–2 Hz; "before" = periodic baseline probe on the direct path;
  "after" = measured through the tunnel.
- **Build:** engine emits metrics over IPC to UI; keep a ring buffer; render with a light
  charting lib (uPlot/Recharts/canvas).

### 5.9 Traffic Shaper (QoS prioritization)
- **What:** prioritize game packets over other bandwidth hogs (downloads, streams, updates)
  on the local link.
- **Looks:** a toggle in Optimization settings; optional per-app priority list.
- **Behavior:** when on, de-prioritizes bulk traffic so the game's small UDP packets aren't
  queued behind big downloads.
- **Build:** local QoS via Windows QoS policies / packet pacing in the WFP layer; cap/pace
  non-game flows when the game is active. (Only shapes egress on the local machine.)

### 5.10 FPS Boost (OS tweaks)
- **What:** free up local resources to raise in-game FPS.
- **Looks:** a toggle; shows a short list of applied tweaks; a "revert" button.
- **Behavior:** reversible, opt-in; warn it's cosmetic-ish and system-dependent.
- **Build:** set process priority/affinity for the game, enable Game Mode / hardware GPU
  scheduling flags, trim background services, set power plan to high performance. **All must
  be reversible** and logged.

### 5.11 Multi-Internet (connection bonding/redundancy)
- **What:** use two connections (e.g. Ethernet + Wi-Fi/4G) at once for redundancy.
- **Looks:** Optimization tab lists detected adapters with checkboxes + a mode (Redundancy /
  Load-balance).
- **Behavior:** duplicate/split the already-duplicated tunnel traffic across both NICs so one
  link dropping doesn't stall the game.
- **Build:** bind separate relay sessions to each local adapter (source-IP binding); feed the
  same dedup-tagged stream over both; egress dedups across all. Great synergy with 5.7.

### 5.12 Route mode vs Proxy mode
- **What:** two ways to capture traffic, for compatibility.
- **Looks:** Network settings radio: "Route mode (recommended)" / "Proxy mode".
- **Behavior:** Route mode = driver-level capture (best, transparent). Proxy mode = app points
  at a local SOCKS/redirect for stubborn games/anti-cheat edge cases.
- **Build:** Route = WFP/WinTUN path. Proxy = local SOCKS5 + per-game proxy injection or
  redirect rules.

### 5.13 Settings — full list
- **General:** launch on Windows start, start minimized, minimize-to-tray, close-to-tray,
  language (i18n), theme (dark default / light), notifications.
- **Network:** route vs proxy mode, network adapter selection, packet-duplication level,
  MTU/encapsulation, kill-switch (block game traffic if tunnel drops instead of leaking
  direct).
- **Optimization:** Traffic Shaper, FPS Boost (+ tweak list), Multi-Internet.
- **Advanced/Diagnostics:** view/export logs, run a manual ping/route test, reinstall/reset
  the network driver, feature flags, send report to support.

### 5.14 Auto-update & notifications
- **What:** silent client updates, catalog/relay refresh, in-app news.
- **Looks:** toast/badge on update; "What's new" panel.
- **Build:** background updater (Squirrel/electron-updater or MSIX), signed binaries; catalog
  and relay registry fetched on launch + periodically.

### 5.15 System tray + in-game overlay
- **Tray:** show/hide, quick-connect last game, pause optimization, quit.
- **Overlay HUD:** optional on-top ping/loss/jitter readout while gaming (Direct3D/ Vulkan
  overlay or a transparent click-through window). Must be anti-cheat-safe (prefer a separate
  borderless topmost window, not game injection).

---

## 6. VISUAL / UX DESIGN SPEC ("how it looks")

> Build your **own** palette and logo. The structure below is the layout language of
> this app category; the specific colors are a suggested *original* scheme, not ExitLag's.

### 6.1 Overall look
- **Theme:** dark-first. App chrome ~ `#0E1116` bg, panels `#161A22`, borders `#232833`.
- **Accent:** one energetic accent for primary actions + "optimized" state. Suggested
  original accent: electric cyan `#19E3B4` or violet `#7C5CFF` (choose your own — avoid
  ExitLag's orange to stay distinct).
- **Text:** `#E6E9EF` primary, `#8A92A6` secondary.
- **State colors:** good `#2ECC71`, warn `#F2C94C`, bad `#EB5757`.
- **Type:** Inter / Roboto; numbers tabular for the stats. Rounded 10–12px cards, soft
  shadows, 150ms ease transitions.

### 6.2 Main window layout (1100×720 default, resizable, min 920×600)
```
┌──────────────────────────────────────────────────────────────────────────────┐
│  LOGO            [ search games … ]                             ⚙ · ☰ · — □ ×│  top bar (56px)
├───────────────┬────────────────────────────────────────────────────────────┤
│ GAME LIBRARY  │  VALORANT                                     ● Optimizing    │
│ ┌───────────┐ │  ┌──────────────────────────────────────────────────────┐   │
│ │All Fav Inst│ │  │ Route:  (•Recommended)  ( By Region )  ( Specific )   │   │
│ └───────────┘ │  └──────────────────────────────────────────────────────┘   │
│ ⭐ Valorant ● │                                                              │
│    LoL        │  ┌── Route cards ───────────────────────────────────────┐   │
│    CS2        │  │ ◉ Recommended (Smart)      58 ms   0.0% loss   3 ms  🟢 │  │
│    Apex       │  │ ○ EU-West via Carrier A    61 ms   0.1% loss   4 ms  🟢 │  │
│    Dota 2     │  │ ○ EU-West via Carrier B    64 ms   0.3% loss   6 ms  🟡 │  │
│    Fortnite   │  │ ○ EU-Central               70 ms   0.0% loss   5 ms  🟢 │  │
│    …          │  └──────────────────────────────────────────────────────┘   │
│               │                                                              │
│               │  [  Analyze  ]                       [  APPLY ROUTES  ]      │
│               │                                                              │
│               │  Before 92ms ──►  After 58ms   Loss 0.0%   Jitter 3ms        │
│               │  ┌───────────── live graph (last 60s) ──────────────────┐    │
│               │  │      ╭╮        ╭─╮                                    │    │
│               │  │ ─────╯╰────────╯ ╰──────── after ───────────────────  │    │
│               │  └──────────────────────────────────────────────────────┘    │
└───────────────┴────────────────────────────────────────────────────────────┘
```

### 6.3 Key components
- **Route card:** radio + label + 3 metric columns (ping/loss/jitter) + quality dot; selected
  state = accent border + subtle glow.
- **Primary button (Apply Routes):** large, accent-filled; turns into "Stop" (danger outline)
  when active; shows spinner during apply.
- **Status pill:** Idle (grey) / Analyzing (blue, pulsing) / Optimizing (accent) / Error (red).
- **Stats strip:** before→after with an arrow; big tabular numbers; color by quality.
- **Live graph:** two series (direct baseline faint, optimized bold); 60s window; 1–2 Hz.

### 6.4 States to design (don't skip these)
Idle · game selected (not analyzed) · analyzing · analyzed/ready · applying · optimizing ·
route degraded (auto-switch toast) · tunnel dropped (kill-switch) · offline/no-backend ·
page not connected to the app window · driver missing/needs reinstall ·
no supported game running · update available.

---

## 7. Screen-by-screen wireframe notes (compact)

- **Route mapping loader:** full-panel spinner + "Mapping the best routes for you…" + progress
  of relays probed.
- **Main:** as §6.2.
- **Settings:** left vertical tabs (General/Network/Relays/Optimization/Advanced), right
  content; Save/Revert footer where relevant.
- **Tray menu & overlay:** as §5.15.

---

## 8. Data model (core entities)

```
Game       { id, name, icon_url, platforms[], smart_available,
             server_ranges:[{cidr, ports[], proto}], route_options[] }
Relay      { id, region, continent, provider, ip, roles:[ingress|transit|egress],
             capacity, health:{rtt, loss, load}, updated_at }
RouteChain { id, game_id, label, hops:[relay_id...], user_region }
Session    { id, game_id, chains_active[], dup_level, started_at }
Metric     { session_id, ts, before_ms, after_ms, loss_pct, jitter_ms, path_health[] }
Favorite   { game_id }   (stored locally; there are no users)
```

---

## 9. Backend API surface (REST/gRPC)

Optional shared services; none of them needs an account.

```
GET  /catalog/games          → full game list (+ server_ranges, route_options)
GET  /relays                 → relay registry (filtered by user region)
POST /routes/candidates      {game_id} → candidate RouteChains
POST /sessions               {game_id, chains, dup_level} → session_id
DELETE /sessions/{id}
POST /telemetry/metrics      ← client pushes anonymized metrics
GET  /app/update             → latest version + signed artifact url
GET  /app/news               → in-app notifications
POST /support/report         ← diagnostics bundle
```

---

## 10. Relay network design

- **Tiers:** ingress (many, close to users) · transit (premium backbone / direct peering) ·
  egress (close to each game's datacenters). A chain = ingress→[transit]→egress.
- **Health:** every relay reports RTT/loss/load; backend prunes unhealthy ones from candidate
  lists; client re-measures top candidates.
- **Dedup:** egress relay owns per-flow sliding-window dedup (first-seq-wins) before delivering
  to the game server; return traffic dedups at ingress.
- **Transport:** your own UDP overlay with header `{flow_id:u64, seq:u32, ts:u32}`; optional
  light encryption (don't over-encrypt tiny packets — adds latency). Keep MTU headroom.
- **Placement:** start with a handful of VPS in key regions (e.g. NA-East/West, EU-West/Central,
  SA-East, SEA, OCE); expand by measured demand.

---

## 11. Client internals (the engine) — build order

```
Game process
   │  (UDP to game server CIDR:port)
   ▼
WFP callout / WinTUN  ── captures ONLY matched flows (by process + CIDR+port)
   ▼
Flow manager  ── per-flow state, sequence numbering
   ▼
Duplicator    ── emit N copies, one per active chain
   ▼
Relay transport (UDP overlay)  ── encapsulate {flow_id,seq,ts}, send to ingress relays
   ▼
… relays forward, egress dedups, delivers to game server …
   ▲
Return path   ── ingress dedups, strips header, re-injects to the game socket
   ▲
Measurement loop (parallel)  ── baseline(direct) + per-chain probes → metrics IPC → UI
```

**Privileged service** does capture + tunnel (runs as admin/SYSTEM). **UI** runs as the user
and talks to the service over a local secured IPC (named pipe) — never put kernel/driver logic
in the UI process.

---

## 12. Connection state machine

```
DISCONNECTED ──select game──► GAME_SELECTED ──Analyze──► ANALYZING ──done──► READY
READY ──Apply Routes──► APPLYING ──ok──► OPTIMIZING ──Stop/game closed──► DISCONNECTED
OPTIMIZING ──path degraded──► OPTIMIZING' (auto-switched, toast)
OPTIMIZING ──tunnel lost──► DEGRADED ──(kill-switch on? block : leak-direct)──► reconnect
ANY ──driver missing──► NEEDS_DRIVER (offer reinstall)
```

---

## 13. Edge cases & error handling (don't forget)
- Anti-cheat compatibility: never inject into game process; capture at network layer only;
  test against Vanguard/EAC/BattlEye.
- Game not in catalog → "Add custom game" (user supplies process + server IPs).
- Multiple games at once · game changes servers mid-match (re-probe) · DHCP/adapter change ·
  VPN already running · IPv6 game servers · firewall blocking the overlay UDP port ·
  relay outage (failover) · clock skew (ts dedup) · MTU/fragmentation · offline mode (cache
  last catalog, disable Apply).

---

## 14. Non-functional requirements
- **Security:** signed driver + binaries; least-privilege service; local IPC authenticated;
  TLS to backend; no accounts or credentials to store; don't log packet payloads.
- **Performance:** engine adds <1–2 ms local overhead; dedup O(1) per packet; UI 60fps,
  metrics ≤2 Hz; memory modest.
- **Reliability:** kill-switch; clean teardown always restores networking; crash of UI must
  not strand the driver (watchdog).
- **Platform:** Win10/11 x64 first; driver signed (EV cert + WHQL path for WFP).

---

## 15. Recommended tech stack
- **UI:** Electron + React + TypeScript (fast, cross-platform, easy charts) — or native
  WinUI 3 / Qt if you want a smaller footprint.
- **Engine (MVP):** Go or Rust user-space using **WinDivert** (quick) → later a real **WFP
  callout driver** (C/C++) or **WinTUN** (Wireguard-style) for production.
- **Relays:** Go (great UDP/networking), deployed on VPS; Anycast optional later.
- **Backend (optional):** Go/Node + Postgres; Redis for relay health; object
  storage for update artifacts.
- **Overlay:** separate topmost transparent window (safe) before considering real overlay APIs.

---

## 16. Build order / roadmap (MVP → full)
1. **MVP core:** one game hardcoded → WinDivert capture → single relay
   → before/after ping readout → Apply/Stop. Prove latency improvement end-to-end.
2. **Multipath + dedup:** add 2–4 chains + egress dedup → show loss/jitter drop.
3. **Catalog + route selection UI:** backend game catalog, route cards, Analyze.
4. **No accounts:** free and open source, so there is nothing to gate (intentionally empty).
5. **Settings suite:** Traffic Shaper, FPS Boost, Multi-Internet, kill-switch, proxy mode.
6. **Polish:** tray, overlay HUD, auto-update, i18n, diagnostics, auto-switch routes.
7. **Hardening:** real WFP driver + signing, anti-cheat testing, relay autoscaling.

---

## 17. Reality check (so the product is honest)
- Helps most when the **ISP route is bad / congested / loss-prone**; multipath reliably cuts
  **jitter and loss** even when raw ping barely changes.
- Cannot beat physical distance if the ISP path is already optimal, and **cannot fix local
  Wi-Fi / bufferbloat** (the tunnel starts after your local link). Set expectations in the UI
  (e.g. a "your bottleneck looks local" hint when baseline jitter is high but route is fine).

---

*End of blueprint. This is the complete idea: every feature, its look, its behavior, its
implementation, and the exact user workflow — enough to design and build the whole app.*
