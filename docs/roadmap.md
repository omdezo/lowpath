# Lowpath roadmap: every gap, every game, every region

Status: plan of record, October 2026. [architecture.md](architecture.md) describes the code
that exists today. Lowpath is free and open source: there are no accounts,
logins, trials or subscriptions, and none are planned.

Notation:
- **[USER]**: the task needs a maintainer (money, a cloud account, a certificate, a company, or being at the PC to approve UAC).
- **S/M/L**: S = 1 to 3 days, M = 1 to 2 weeks, L = 3 or more weeks of one engineer.
- **UNVERIFIED**: from a secondary source or inference; check before relying on it.

---

## 0. The short, honest answer

ExitLag's "~9 ms to AE Dubai01" (relays Dubai29/Dubai03, Fujairah01, Dubai05) has three ingredients:
1. Relays physically in the UAE, peered on UAE-IX and Fujairah carrier hotels.
2. A catalog that knows which Riot address is "AE Dubai01".
3. Routing that tries in-country hops first ("Use local routes first").

The 9 ms is ExitLag's own *estimate*, not a measured end-to-end figure (UNVERIFIED how it is computed).

Lowpath already has a correct core: per-process capture, multipath duplication, first-copy-wins dedup and a keyed relay. What it lacks is everything around that core:
- relays (none exist)
- knowledge of game servers before play
- TCP
- anti-cheat-safe capture
- loss and jitter diagnosis
- a real install, update and signing story
- a way to pay for bandwidth if a shared fleet is run

None of this is Dubai-specific. The same machinery has to work for a player in São Paulo, Manila or Johannesburg on any of about 60 popular PC games (§4, §4.1), plus emulated mobile games and any `.exe` the user adds.

Two facts shape the whole plan:

1. **The Gulf changes under your feet.** In March 2026 strikes damaged AWS ME-CENTRAL-1 (UAE) and disrupted ME-SOUTH-1 (Bahrain) ([DCD](https://www.datacenterdynamics.com/en/news/aws-uae-outage-after-objects-struck-the-data-center-cause-fire-amid-iran-attacks/), [heise](https://heise.de/-11222079)). VALORANT's ME players were reportedly moved to Riyadh and Mumbai ([Dot Esports](https://dotesports.com/valorant/news/valorant-riyadh-mumbai-middle-east-servers), secondary), which matches the user's client: Riyadh 35, Mumbai 41, no Bahrain. **But Riot's own server-select page (dated 1 Jun 2026) still lists Bahrain under EU and no Riyadh** ([Riot](https://support.riotgames.com/en-us/valorant/support-tools/server-select), fetched Oct 2026). Official lists lag reality. So the catalog has to be **live and health-checked** and must trust what the game actually connects to over any published list, never a static "Dubai is best" list.
2. **Duplication only fixes loss after the paths split.** If loss is on the user's Wi-Fi or ISP access line, every copy dies together. The product has to *find* where loss happens and say so, not just add copies. With one uplink, the only fixes for home-side loss are time-shifted copies (short bursts) and **Multi-Internet** (a second physical link). That is why Multi-Internet moved up to M4 (§5).
3. **Dubai is one case, not the product.** Every rule in this plan is written per region and per game: §4 and §4.1 list about 60 titles, and §4.2 gives a playbook for every player region from Muscat to São Paulo, Manila and Lagos. A Gulf-only fleet is the first milestone, not the design.

### 0.1 What the user needs first (ordering rules)

The backlog in §5 follows four rules. They exist because earlier drafts got the order wrong.
1. **Testable without being at the PC.** Testers often drive sessions remotely and can't click UAC prompts. A dev-mode `LowpathSvc` install (one UAC, once) therefore comes in **M0**, not M7. Otherwise no later Muscat test can run unattended.
2. **Start the long-lead paperwork on day 1.** Code-signing identity, a company (for the driver), and cloud accounts each take weeks. They run as a parallel **[USER]** track from M0 (§5 "Parallel track"), not in the milestone where they are first needed.
3. **Measure before building relays, but ship a visible win early.** M1 gives server lists with live ping for every game that has a public beacon (Valve, Epic, GameLift, PlayFab), plus flow-learned servers for the rest. A player can see "what server am I on and is it my Wi-Fi" for every game before any relay exists.
4. **Never test anti-cheat on an account the user cares about.** Anti-cheats ban in delayed waves. Every anti-cheat test uses a secondary account and gets a 30-day follow-up check (§2.3 C2).

---

## 1. What exists today (from the code)

| Area | Today | Where |
|---|---|---|
| Capture | Windows: WinDivert 2.2.2, outbound IPv4 UDP only, per process via `GetExtendedUdpTable` + Toolhelp, inject inbound on the same interface. Linux: TUN device, nftables mark (socket owner from `/proc`, kernel fast path for other programs' ports), `ip rule` fwmark table, replies written into the TUN (#41) | `internal/capture`, `internal/divert`, `internal/netlink` |
| Overlay | v2 framing `{magic,type,token,flow,seq,ts,addr}`; token = SHA-256(key)[:8], sent in clear; no encryption, MAC, fragmentation or TCP stream type | `internal/proto/proto.go` |
| Multipath | 2 to 4 copies chosen by loss/jitter (`copies()`), score `RTT + 3·J + 8·loss` | `internal/engine/engine.go`, `tracker.go` |
| Dedup | 1024-seq sliding window with `RestartGap` | `internal/dedup` |
| Relay | Egress (dedup, deliver, ICMP server leg, `IsPublic` filter, flow caps) and Forwarder hop; systemd install over SSH | `internal/relay`, `cmd/relay`, `deploy/` |
| Measurement | ICMP before; probe RTT + egress ICMP leg after; says "server ignores ping" honestly | `internal/engine/baseline.go`, `internal/icmp` |
| Catalog | 22 games: process names, Steam app id, install paths. **No server pools, regions, ports, prefixes or login hosts** | `internal/app/games.json`, `catalog.go` |
| Server knowledge | Learned from live flows only; busiest flow is primary and remembered | `internal/engine/flows.go` |
| App shell | `Lowpath.exe` (WebView2 + React), localhost API with per-launch secret. Without the service it runs the engine itself, elevated. With the dev-mode `LowpathSvc` (`lowpathsvc install`, one UAC, starts at boot) it stays unelevated and passes the API to the service over `\\.\pipe\lowpath` (#6). No tray, update, signing or installer | `cmd/lowpath`, `cmd/lowpathsvc`, `internal/app`, `internal/shell`, `ui/` |
| Licence | MIT, free, no accounts | `README.md` |

---

## 2. Gap register

Each row gives:
- **Why:** what the player feels.
- **Today:** what the code does now.
- **Target:** the behaviour to build.
- **Accept:** a testable pass criterion.

### 2.1 Relays and network

| # | Gap | Why | Today | Target | Accept |
|---|---|---|---|---|---|
| N1 | No relay fleet | No relays means no speedup; the loopback relay can't beat direct | Bring-your-own relays plus loopback | Two providers per metro in about 35 metros (Valve POP metros + GCC) | From Muscat, at least 2 healthy relays each in UAE, KSA, India and EU; registry shows 99% heartbeat uptime over 7 days |
| N2 | No in-country ingress ("local routes first") | First hop over the ISP's transit is where the local ISP loses ms | None | Ingress relays in Muscat (Oman-IX/MC1), Fujairah, Dubai; prefer relays in the user's country or ASN | `local_first=on` picks an OM/AE ingress when it is within 2 ms of the best |
| N3 | No relay registry | Can't discover, health-check or drain relays | Static list in `state.json` | `registry` service: register, heartbeat, signed relay list, mesh matrix | A killed relay leaves the signed list within 30 s and clients re-route without a user action |
| N4 | Static shared key, token in clear | Anyone who sniffs one packet can use the relay as an open UDP proxy | `TokenFromKey(key)` on every packet | Ed25519 session tickets (≤ 15 min) + per-session AEAD key; destination allowlist | Replaying a captured packet after ticket expiry is dropped (relay test); sending to a non-catalog IP is dropped |
| N5 | No DDoS or abuse protection | Relays get hit; a leaked relay becomes a reflector | Flow caps, `IsPublic` | XDP pre-filter (native mode where the NIC driver supports it, generic otherwise), per-source token bucket, cookie challenge under load. Volumetric floods larger than the VM's link can only be absorbed upstream, so egress relays for high-profile games sit on providers with game DDoS filtering (OVH Game, DataPacket) | Lab: first measure the VM's own pps ceiling (cheap VPS hypervisors often cap well below 1 Mpps, UNVERIFIED per provider). A spoofed flood at 80% of that ceiling keeps legit session loss < 0.5%. Above link capacity, the client fails over to another relay within 2 s |
| N6 | No relay-to-relay mesh measurement | Can't pick a better middle leg | Forwarder exists, unmeasured | Every relay probes region peers and hubs every 30 s | Registry `GET /mesh` shows p50/p95/loss for every pair in a region, < 60 s old |
| N7 | No region failover | When Bahrain disappears the user is stuck | None | Catalog marks pools `offline`; client re-targets the next pool and tells the user | Mark a pool offline in the catalog; client shows "Bahrain offline, using Riyadh" within 5 min, no restart |
| N8 | IPv4 only | IPv6-preferring ISPs and games bypass Lowpath | Capture filter `ip`, v4 only | Capture and relay IPv6 (proto already carries 16-byte addresses) | lpcheck over IPv6 shows the relay's v6 address |
| N9 | Single uplink | Can't beat home-side loss; no Multi-Internet. ExitLag ([KB 203](https://www.exitlag.com/knowledgebase/203)) and NoPing (up to 6 links, [NoPing](https://noping.com/features/multi-connection)) both ship it | One socket per path on the default route | Bind paths to specific interfaces (Ethernet + Wi-Fi + USB phone) with `IP_UNICAST_IF`; per-link data counter; a metered link (`GetConnectionCost` / `NLM_CONNECTION_COST`) carries only the duplicate copy, and only when the main link shows loss | Pull the Ethernet cable mid-session: zero game-visible gap > 100 ms when Wi-Fi is also bound. On a metered phone link, a clean 1-hour session uses < 5 MB of phone data |
| N10 | No relay for unusual destinations | Community servers (Rust, DayZ, Minecraft, FiveM) live on any hoster in any country | Egress only where we have relays | "Nearest egress to destination": geo-locate the destination IP, pick the relay POP with the lowest measured RTT to it from the mesh, and fall back to direct with an honest label | A Rust server in a random Frankfurt hoster gets an egress in FRA automatically; a server in a metro with no relay shows "Direct only" |

### 2.2 Loss, jitter, latency engine

| # | Gap | Why | Today | Target | Accept |
|---|---|---|---|---|---|
| L1 | No per-path delivered/lost accounting | The user can't see that multipath saved them | Probe-based loss only | Egress keeps a per-path receive bitmap on data seqs, reports `p_A, p_B, p_AB, ρ` every 1 s | UI shows "Lane A 2.1% · Lane B 1.4% → delivered 0.03%" matching netsim ground truth ±0.1% |
| L2 | No burst protection on a shared segment | Wi-Fi bursts kill both copies | Spatial duplicates sent at the same instant | Time-shifted duplicates: copy B after d = clamp(mean burst, 4, 20 ms), using a high-resolution waitable timer | netsim Gilbert-Elliott burst (ABL 8 ms, 3%) on a shared first hop: effective loss drops ≥ 5× vs simultaneous copies |
| L3 | Redundancy policy is coarse | Wastes bandwidth, or protects too little | 2/3/4 copies by thresholds | Policy table in §3.3: step up at once, step down after 60 s clean; disjoint pair choice by `p_A·p_B` | Unit tests on synthetic traces; no flapping more than once per minute |
| L4 | Weak hysteresis / path cost | Ping "jumps", switching causes spikes | Score `RTT+3J+8loss` | `cost = p50 + 2J + 1.5(p95−p50) + 40·loss%`; switch only for > max(3 ms, 10%) sustained 10 s; drop on > 5% loss over 3 s | Replay test: ≤ 1 primary switch per 10 min on a stable trace |
| L5 | No late-copy bound | Stale copies inflate stats | Window by seq only | Drop copies > 250 ms after the first; count `late_copies_dropped`, `too_old` per path | Counters exposed in `/api/session`, unit test |
| L6 | No MTU handling | Big connect/voice bursts fragment or vanish | `MaxOverhead` known, no PMTU | PMTU probe with DF per relay; Lowpath-level fragmentation over the limit; MSS clamp for TCP | 1472-byte UDP payload through a 1420-MTU path arrives intact (test) |
| L7 | No FEC option for high-rate flows | Voice and bulk TCP-over-overlay benefit from FEC | None | Optional XOR/RS(k,m) for flows > 200 pps or TCP streams (`klauspost/reedsolomon`) | Off by default; when on, 2% random loss → < 0.2% at +25% bandwidth |
| L8 | DSCP / home QoS | Upstream Wi-Fi contention | None | `QOSSetFlow` DSCP 46 on outer packets, toggle | Wireshark shows DSCP EF on outer packets when on |
| L9 | Wi-Fi scan spikes | Multi-second hiccups on Wi-Fi | None | `wlan_intf_opcode_background_scan_enabled=FALSE` + `media_streaming_mode=TRUE` during sessions, reverted after | `netsh wlan show interfaces` plus 20 Hz gateway ping: no periodic 100 ms+ spikes during a boost |
| L10 | Our numbers are not checked against the game's | Players trust the in-game meter, not ours. If the two disagree, we lose them | None | Every lab and field test also records the game's own counters: VALORANT video settings → Stats (network RTT, packet loss), CS2 Settings → Game → Telemetry (ping, packet loss, jitter; [Steam](https://help.steampowered.com/faqs/view/5E6F-5B36-5485-F6B9)), Fortnite net debug stats, Apex `cl_showpos`/net graph (UNVERIFIED names). Store them next to Lowpath's figures in the session report | For CS2 and VALORANT, the in-game loss % and our "delivered" loss agree within 0.5 points over a 20-minute match, and the in-game ping is within 5 ms of our end-to-end figure |
| L11 | Upload and download loss are lumped together | Choke on upload (game → server) and download (server → game) have different fixes | One loss figure | Report loss per direction: upstream from the egress bitmap, downstream from the client's dedup window (the egress already duplicates downstream over every live path, `internal/relay/egress.go`). Redundancy policy runs per direction | netsim with 3% loss on the downlink only: the UI shows "download loss", and downstream lanes step up while upstream stays at 2 |
| L12 | No latency budget for Lowpath itself | A booster that adds 1 ms of its own processing eats the gain | Not measured | Budget: capture → send ≤ 0.2 ms p99, receive → inject ≤ 0.2 ms p99, CPU < 2% of one core at 128 pps, RAM < 150 MB for the service. Benchmarks run in CI | `go test -bench` over the engine path plus an ETW trace on the Muscat PC stay within the budget; a regression fails CI |

### 2.3 TCP and capture

| # | Gap | Why | Today | Target | Accept |
|---|---|---|---|---|---|
| C1 | No TCP ("Redirect login") | Launchers and logins hang; LoL, WoW, PoE2, Minecraft Java are TCP | UDP only | Split TCP: local proxy → reliable overlay stream → egress dials the server; 1 path, `TCP_NODELAY`, BBR on relays | WoW/PoE2 play through a relay; `GetPerTcpConnectionEStats` shows retransmits before vs after |
| C2 | WinDivert is an anti-cheat liability | EAC acted on the WinDivert service in 2026 (Rust and Fortnite user reports, one claimed ban) ([Flowseal #16128](https://github.com/Flowseal/zapret-discord-youtube/discussions/16128), [#16097](https://github.com/Flowseal/zapret-discord-youtube/issues/16097)) | WinDivert for everything | Backends: `wfp` (own driver, later) > `tun` (WinTUN) > `divert` (dev and non-anti-cheat games only); compat gate per game | On **secondary accounts only**: VALORANT, LoL, Fortnite, Apex, R6, PUBG, CS2+FACEIT, BO7, Battlefield 6, ARC Raiders each complete 20 matches over 14 days in `tun` mode with no anti-cheat error, and **no ban after a 30-day follow-up** (anti-cheats ban in delayed waves). Re-run within 7 days of every major anti-cheat update (lab log) |
| C3 | Process identity is name-only | A renamed exe or a launcher helper is missed | Image name match | Match on full path + Authenticode signer; follow child processes; catalog `exe[]` with publisher | Riot client spawning `VALORANT-Win64-Shipping.exe` is captured on the first packet |
| C4 | No ICMP routing | Ping-based in-game meters and some games use ICMP. ExitLag exposes a separate ICMP route count ([Softpedia](https://www.softpedia.com/get/Gaming-Related/ExitLag.shtml)) | Not routed | In `tun` mode, answer ICMP echo for routed prefixes by relaying it as an overlay probe; ICMP lanes 0 to 2 in Advanced | `ping <game ip>` while boosted shows the relayed RTT |
| C5 | No fail-open guarantee | If Lowpath crashes mid-match the player must not lose the match. This is worse than any lag | `Close()` restores networking only on a clean exit | Service watchdog: on engine panic, driver unload or service kill, remove WinDivert handles, tun routes and WFP filters within 1 s, so traffic goes direct. Boot-time cleanup of leftover routes and filters | `taskkill /F` on the service mid-match: the game keeps playing (UDP) or reconnects (TCP) within 2 s; after a forced power-off, the next boot has no stale `/32` routes |
| C6 | Boost while the game is already running | Most players hit Boost after the game is open | Works for UDP (new packets are captured) | UDP: pick up live flows at once. TCP: established connections are not moved (moving them breaks them), so the UI says "Login/chat stay direct until you restart the game". `tun` mode adds routes for the live server IPs | Start VALORANT first, then Boost: the game flow moves to the relay within 2 s with no disconnect, and the TCP notice is shown |
| C7 | Conflicts with other boosters and VPNs | ExitLag/NoPing leave WinpkFilter drivers behind ([MS Q&A](https://learn.microsoft.com/en-us/answers/a/5471382)); a full VPN steals the default route | None | Detect running VPN adapters, WinpkFilter/`ndisrd`, other WinDivert users and ExitLag/NoPing services. Show "close X first" with a one-click explanation; never fight another driver for the same packets | With ExitLag running, Boost shows a clear conflict message instead of a silent failure |

### 2.4 Game and server catalog

| # | Gap | Why | Today | Target | Accept |
|---|---|---|---|---|---|
| G1 | No server pools | Can't pick "AE Dubai01" before playing | Learned flows only | `ServerPool` model (§3.5) for the ~60 titles in §4 and §4.1, all regions | Game page lists pools with direct ping before launch for ≥ 80% of them; the rest show learned servers after one match |
| G2 | No live discovery APIs | Static lists rot | None | Valve `GetSDRConfig`, Epic `ping-*.ds.on.epicgames.com`, GameLift UDP 7770 beacons, PlayFab QoS UDP 3075, RIPEstat prefixes | Hourly refresh job; diff report; CS2 shows `dxb` ≈ 17 ms from Muscat |
| G3 | No match-server following | User picks Dubai, matchmaker sends them to Riyadh | Primary flow is remembered | "Auto (follow game)": detect the real match server, map it to a pool, re-route to the best egress for *that* pool within 2 s; pre-warm candidates (GearUP AIR / NoPing AI Swap style) | Change Fortnite region mid-session; the route diagram changes egress within 5 s, no stall > 300 ms |
| G4 | No crowd learning | Unknown games and new servers stay unknown | Local memory | Opt-in telemetry `{game, dst /24, port, ASN, client ASN, rtt/loss}` → promote learned pools at ≥ 20 confirmations | A new /24 seen by 20 test clients appears in the signed catalog as `confidence: high` |
| G5 | Coverage of 22 games, no long tail | "Ultimate for all games" | 22 entries + custom exe | 100 curated + any exe via flow learning; in-app "request a game" | Any UDP or TCP game added by exe is routed and its server labelled after one match |
| G6 | No launcher library | User must find the game | Steam folders + paths | Steam ACF, Epic manifests, Riot YAML, Battle.net, EA, Ubisoft, Xbox `.GamingRoot`, plus process polling | Fresh PC with 5 launchers: ≥ 95% of installed games found in < 3 s |
| G7 | Mobile games on PC emulators | PUBG Mobile, Free Fire, CoD Mobile and MLBB are huge in the GCC, South Asia, SEA and LATAM, and are played on BlueStacks, LDPlayer, MuMu and Google Play Games for PC | Not detected | Catalog entries for emulator processes (`HD-Player.exe`, `dnplayer.exe`, `MuMuPlayer.exe`, Google Play Games `crosvm.exe`; names UNVERIFIED) with per-game flow classification inside the emulator's traffic | PUBG Mobile in BlueStacks: the match flow is found and routed; emulator store and ad traffic stay direct |
| G8 | Catalog breaks on game patches | A patch renames the exe or moves ports and the boost silently stops working | Manual `games.json` | Nightly CI job: launch-free checks (Steam ACF build IDs, Epic manifest versions) flag changed titles; crowd telemetry "game detected but no game flow found" alerts within 1 h | A renamed exe in a test build triggers an alert, and a catalog fix ships without a client release |
| G9 | Non-game apps the player cares about | Discord voice turning robotic mid-match; launcher downloads; streaming upload. ExitLag says it also optimizes voice apps, browsers, launchers and emulators ([KB 273](https://www.exitlag.com/knowledgebase/273)) | Games only | Optional "Voice" profile: Discord voice UDP (RTC servers on 50000–65535, UNVERIFIED) on 1 lane with duplication; launchers stay direct by default | With 2% induced loss, Discord voice stays clear with the Voice profile on (blind A/B with two listeners) |
| G10 | Game region vs player region | Many players pick a far region on purpose (friends abroad, KR/JP-only games, bigger queues). GearUP sells this ([GearUP](https://www.gearupbooster.com/support/how-to-use-gearup-booster.html)) | Not modelled | Server picker shows every region the game offers, not only the nearest; the route solver uses long-haul backbone legs (§3.4). **No geo-unblocking**: Lowpath never presents a fake country to get around a publisher's region lock (ToS and legal risk, §6) | From Muscat, a "VALORANT AP: Mumbai/Singapore" or "LoL EUW" choice shows est. and measured ping and routes there |

### 2.5 Diagnostics and honesty

| # | Gap | Why | Today | Target | Accept |
|---|---|---|---|---|---|
| D1 | No "why am I lagging" | Most lag is local and no competitor says so | Verdict in Settings → Diagnostics from the bufferbloat test, route trace, boost and route analysis, with confidence and a text export (#14); 20 Hz gateway pinger and the segmented Home / ISP / Lowpath / Server bar (#10); Wi-Fi signal (#11) still to come | Segmented view: Home / ISP / Lowpath / Game server, with a verdict engine (§3.6) | On a laptop 10 m from the AP with a microwave on: verdict "Your Wi-Fi is the problem", gateway loss shown |
| D2 | No bufferbloat test | Downloads kill ping | 20 s test in Settings → Diagnostics, passive banner during a boost (#12) | 20 s active test, grade A+ to F; passive detector during play | Start a Steam download mid-session; banner "something on your network is filling the connection" within 10 s |
| D3 | No MTR | Can't prove where loss starts | Route trace in Settings → Diagnostics (#13): ICMP TTL 1 to 30, 200 samples/hop, UDP mode to a relay port, persistence rule, text export | `IcmpSendEcho2Ex` TTL 1 to 30, ≥ 200 samples/hop + UDP mode; rule: loss counts only if it persists to the destination | Exported report matches WinMTR on the same target ±1% loss per hop |
| D4 | Before/after can be unfair | "Ping got worse" is the top complaint for every competitor | ICMP before vs probe after | Same method, same target, 60 s each; show effective loss; auto-recommend "run direct" when the relay route is not better | When direct is within 2 ms and no lossier, UI says "Already optimal, Lowpath not needed" |
| D5 | No in-game view | Players don't alt-tab | None | Click-through topmost window (no injection); phone view over LAN | Overlay visible over borderless CS2; no DLL loaded into `cs2.exe` (Process Explorer) |
| D6 | No support bundle | Can't debug user issues | Log file | "Report a problem": last 5 min metrics + logs + diagnostics, redacted | One file < 2 MB, no full IPs beyond /24 for third parties |

### 2.6 UX and product shell

| # | Gap | Why | Today | Target | Accept |
|---|---|---|---|---|---|
| U1 | UAC every launch | Annoying; blocks autostart | Single elevated process | `LowpathSvc` (LocalSystem) + unelevated UI over a named pipe | Reboot → tray appears, no UAC, Boost works |
| U2 | No route diagram per hop | Trust and understanding | Single line | You (your city) → Fujairah01 → Dubai05 → Riot AE, ms per leg, stacked lanes | Diagram legs sum to within 2 ms of measured total |
| U3 | No location/ISP | Can't explain or prefer local | None | Relay STUN echo + ASN lookup: "<city> · <ISP> (<ASN>)" | Shows correct ASN on the user's PC |
| U4 | Controls missing | Power users need them | Re-optimize buried | Re-optimize, Reset, Advanced: UDP 1 to 4, TCP 0 to 2, ICMP 0 to 2, local first, redirect login, duplication off/smart/always, bandwidth cap, pin/exclude relay | Each setting changes `/api/session` config (unit test) |
| U5 | No tray, toasts, autostart | Expected basics | "Coming soon" | `Shell_NotifyIconW` tray, AUMID toasts, HKCU Run | Toast "Route switched to Dubai05" appears |
| U6 | Arabic / RTL | Main market (GCC) | English | `dir=rtl`, logical CSS, `<bdi dir=ltr>` numerals, ICU plurals, bundled Noto Sans Arabic | Full UI pass in Arabic with no clipped strings |
| U7 | Traffic shaper | Keep other apps from bloating the link | None | Per-app up/down caps (WFP or WinDivert pacing), Smart mode, Delivery Optimization throttle toggle. ExitLag rebuilt its shaper with 5 levels, Smart Mode, per-app caps and an exception list in v5.18 ([ExitLag](https://www.exitlag.com/blog/exitlag-version-5-18/)) | Cap Chrome to 2 Mbps: speedtest in Chrome ≈ 2 Mbps while the game ping stays flat |
| U8 | Other languages | ExitLag's biggest markets are Brazil and LATAM; boosters sell heavily in Turkey, Russia/CIS and SEA | English, Arabic planned | Ship string tables for pt-BR, es-419, tr, ru, id, fil, hi after Arabic; community translation file in the repo | Every screen renders in pt-BR and tr with no clipped strings (screenshot diff in CI) |
| U9 | No history | "Is it worse at night?" and "which server is best for me?" need history | Last session only | Per game × server × hour: ping p50/p95, jitter, delivered loss, direct vs boosted. "Best time and server" hint once 10+ sessions exist | After 10 sessions the game page shows a 24-hour heat strip and the best server for this ISP |
| U10 | Bandwidth cost hidden | Duplication multiplies data use; players on capped or phone plans get surprised | Overhead counter only | Show MB per hour per game in the live view and session summary; warn before boosting on a metered connection | Session summary shows data used within 5% of the NIC counters |
| U11 | No way to ask for a game or get help | ExitLag adds games on request from in-app chat ([GamingTrend](https://gamingtrend.com/interviews/lucas-stolze-explains-lag-fighting-software-exitlag-2)) | GitHub issues | "Request a game" button that sends exe name, launcher and a learned-server summary (consented); "Report a problem" bundle (D6) | A request reaches the catalog queue with enough data to write the entry without contacting the user |
| U12 | Consoles and phones get nothing | Console players are a big part of CoD, EA FC, Fortnite and Rocket League (share UNVERIFIED); GearUP and WTFast sell router and console modes ([ASUS + GearUP](https://press.asus.com/news/press-releases/asus-gearup-console-booster-apac)) | PC process capture only | Step 1 (cheap): "PC as gateway". The console joins the PC's Windows Mobile Hotspot or an Ethernet ICS bridge, and the engine captures that subnet by source IP instead of by process. Step 2 (M9): an OpenWrt package and a mobile app | A PS5 on the PC hotspot plays EA FC through the Dubai relay; the live view shows the console as its own device |

### 2.7 Install, update, signing, privacy

| # | Gap | Why | Today | Target | Accept |
|---|---|---|---|---|---|
| I1 | Unsigned exe | SmartScreen warning; Smart App Control blocks it outright | Unsigned | Cheapest first: (a) **SignPath Foundation**, free OV-level signing for qualifying open-source projects, which an MIT client may be (eligibility UNVERIFIED, apply in M0); (b) OV cert on a cloud HSM ($150–300/yr). Azure Artifact Signing is not available to Omani organizations or individuals ([MS, updated Aug 2026](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options)). Also submit the signed installer to the Microsoft Store (Win32 MSI/EXE), where Store installs show no SmartScreen prompt (same source). EV only for the driver track (M8) | `signtool verify /pa` passes on all binaries; a Smart App Control-enabled Win11 VM runs the installer |
| I2 | No installer | Manual `dist\Lowpath.exe` | None | Inno Setup per-machine, service install, Start shortcut with AUMID, clean uninstall that removes the driver/service | Install → uninstall leaves no service, driver, or `C:\Program Files\Lowpath` |
| I3 | No auto-update | Users stay on broken builds | None | Service fetches Ed25519-signed `stable.json`, `WinVerifyTrust` + pinned publisher, updates only between sessions, staged rollout + rollback | Publish v+1 to beta: test PC updates within 1 h, never mid-boost |
| I4 | No crash reports | Blind to failures | Log | Sentry Go (EU region) + WER LocalDumps, opt-in | Forced panic appears in Sentry with no PII |
| I5 | Own WFP driver | Lowest anti-cheat risk, native TCP redirect | None | `lowpath.sys` WFP callout, attestation-signed, then WHCP | Loads with HVCI + Secure Boot on Win11 25H2; passes Driver Verifier |
| P1 | Privacy | Telemetry is the moat but must be consented | None | Opt-in, client IP truncated to /24, 90-day raw retention, `DELETE /v1/me`, data sheet (GDPR, Oman PDPL RD 6/2022 UNVERIFIED applicability) | Telemetry off → zero bytes to telemetry endpoint (proxy capture) |
| R1 | Legal position per country | A tunnel can look like a VPN. UAE law punishes using a false or third-party IP to commit or hide a crime, including reaching blocked gaming or calling apps, with fines reported at AED 500k to 2M ([Khaleej Times](https://www.khaleejtimes.com/business/tech/vpn-uae-6-million-downloads-dh2-million-fine-misuse?amp=1), secondary). Oman's TRA warned in 2025 that unauthorized VPN use may breach licensing rules ([MEA Tech Watch](https://meatechwatch.com/?p=55356), secondary) | Not considered | Design rules: game traffic only (catalog destinations, never general browsing); relays never used to reach services blocked in the user's country; no "change country" marketing; a per-country policy flag in the catalog. Legal review in Oman, UAE and KSA before running public relays there. Hosting providers refuse sanctioned countries (for example Iran and Syria under US rules), so public relays can't be offered there (UNVERIFIED per provider) | A written legal memo per launch country; `dst_allow` on relays rejects a non-catalog destination (N4 test); the marketing copy check list has no unblocking claims |

---

## 3. Architecture

### 3.1 Processes

```
LowpathSvc.exe (LocalSystem service, Go)
 ├─ capture   CaptureBackend {wfp | tun | divert}
 ├─ engine    UDP mux (dup/time-shift/FEC) + TCP stream proxy
 ├─ catalog   signed catalog cache + discovery probes + flow learner
 ├─ diag      gateway pinger, WLAN, bufferbloat, MTR, TCP EStats
 ├─ updater   signed manifest, WinVerifyTrust, Inno silent install
 └─ pipe API  \\.\pipe\lowpath (ACL: Interactive Users)  ← same JSON API as today
Lowpath.exe (user, unelevated): WebView2 UI + tray + toasts + overlay window
```

Moving `internal/app/server.go` from HTTP-on-localhost to a named pipe also retires the per-launch secret. Keep HTTP for `-demo` and dev only.

### 3.2 Capture layer and fallback order

```go
// internal/capture/backend.go
type Backend interface {
    Name() string                        // "wfp" | "tun" | "divert"
    Start(Target) error                  // Target{Exes []ExeID, Prefixes []netip.Prefix, Ports []PortRange, TCP bool}
    UDP() <-chan Packet                  // captured game datagrams
    InjectUDP(local, remote netip.AddrPort, b []byte) error
    AcceptTCP() (net.Conn, OrigDst, error) // redirected TCP connections
    Stats() Stats
    Close() error                        // must restore networking
}
```

| Order | Backend | Use | Mechanism |
|---|---|---|---|
| 1 | `wfp` (M8) | Default once signed | Own `lowpath.sys` callout: `ALE_CONNECT_REDIRECT_V4/V6` filtered by `FWPM_CONDITION_ALE_APP_ID`, redirect to `127.0.0.1:<port>`, original destination via redirect context ([MS](https://learn.microsoft.com/nb-no/windows-hardware/drivers/network/using-bind-or-connect-redirection)). UDP through `ALE_BIND_REDIRECT` plus a connect-redirect to the local mux |
| 2 | `tun` (M3) | Default for anti-cheat titles until `wfp` ships | WinTUN 0.14.1 ([wintun.net](https://www.wintun.net/)) via `golang.zx2c4.com/wireguard/tun`; `/32` routes for catalog prefixes plus learned server IPs; gVisor netstack for TCP ([tun2socks](https://cdn.jsdelivr.net/gh/xjasonlyu/tun2socks@main/README.md)); per-packet PID check via `GetExtendedUdpTable/TcpTable`, non-game packets re-sent on the physical NIC |
| 3 | `divert` (today) | Dev, non-anti-cheat games, unknown-server discovery | Existing code. Auto-disabled when `vgk`, `EasyAntiCheat*`, `BEService` or `FACEIT*` services run, and for any catalog title whose `anti_cheat` is not `none`/`vac` (covers Javelin, Ricochet, ACE and others whose service names are not yet confirmed) |
| never | LSP / DLL injection / proxy into the game | — | Exactly what anti-cheats ban |

**Compat gate** (`internal/compat`):
- The per-game profile `{anti_cheat, allowed_backends[]}` comes from the catalog.
- On a detected block (Vanguard tray notice, EAC exit code, game exits within 30 s of boost) it falls back 1 → 2 → direct and logs a telemetry event.
- **Discovery gap in `tun` mode:** routes must exist before the first packet, so for an unknown server the client can't route what it hasn't seen. Fix: route the catalog prefixes (AS6507 Riot, AS32590 Valve, cloud ranges per region) up front. For truly unknown games, learn the server from the first session through ETW `Microsoft-Windows-Kernel-Network` events (per-PID remote endpoints, no packet driver, safe next to anti-cheat), or in `divert` mode for non-anti-cheat games. `GetExtendedUdpTable` alone is not enough: it reports remote endpoints only for connected UDP sockets, and many games use unconnected ones.

### 3.3 Overlay protocol v3 (`internal/proto`)

Changes, all version-negotiated in a new handshake:

1. **Handshake** `THello{ticket}` → `THelloOK{session_id, cookie?}`. The ticket is an Ed25519-signed blob from the registry: `{user, session, exp ≤ 15 min, relay_set, dst_allow[], pps/bps caps, aead_key_enc}`. Under load the relay answers with a WireGuard-style cookie (HMAC over the source IP) first.
2. **Data header** keeps `flow, seq, ts` and adds:
   - `path_id u8` (which lane, for per-path accounting)
   - `copy u8` (0 = first, 1 = time-shifted)
   - `frag u8/u8` (Lowpath fragmentation)
   - a 16-byte AEAD tag (ChaCha20-Poly1305, nonce = session ‖ seq ‖ path)

   This replaces the clear-text static `Token`.
3. **`TStats`** (egress → client, 1 Hz): per-path received bitmap summary `{p_A, p_B, p_AB, ρ, burst p01/p10, RFC 3550 jitter per path, late_copies, too_old}`.
4. **`TStream`** (TCP tunnel): one reliable stream per redirected TCP connection, carried over QUIC (`quic-go`) or a framed TCP connection to the egress, with header `{orig_dst, proto}`. Pinned to one path, ideally the same egress as the UDP flow so the server sees one source IP (inference).
5. **`TPing`** (ICMP relay in `tun` mode) and **`TMTU`** probes with DF.

**Redundancy policy** (engine; tune with field data):

| Last 10 s, worst lane | Action |
|---|---|
| loss < 0.1% and J < 3 ms | 2 lanes |
| 0.1 to 1%, ρ ≈ 1 | 2 most-disjoint lanes (lowest `p_A·p_B`, different facility/upstream ASN) |
| > 1%, ρ ≈ 1 | 3 lanes |
| ρ > 5 (shared loss) | Time-shifted copies on, run local diagnostics, show "home network" verdict if the gateway is lossy |
| effective loss > 2% for 30 s | Re-optimize: new discovery, consider another egress region |

Step up at once; step down after 60 s clean. Never buffer to reorder: game netcode handles reordering, and a reorder buffer adds jitter. Only measure the reorder rate. Never add artificial delay to the first copy (lag-switch optics with anti-cheat).

### 3.4 Relay network and registry (`cmd/registry`, `internal/registry`)

- **Registry** (Go + Postgres + Redis), endpoints:
  - `POST /relays/register` (bootstrap token → mTLS cert)
  - `POST /relays/{id}/heartbeat` every 10 s: CPU, pps, sessions, NIC drops, version
  - `GET /relaylist`: Ed25519-signed, cached 5 min by clients
  - `POST /sessions` → ticket
  - `GET /mesh`
  - `POST /telemetry`
  - `GET /catalog`: signed, ETag
- **Mesh:** every relay probes its region peers and hubs every 30 s (20 packets each: p50/p95/loss/jitter).
- **Egress probing:** the egress measures the game server at session start (ICMP, else a UDP or TCP-SYN probe where possible).
- **Route selection:** `client→ingress (measured) + mesh legs + egress→server`, loss penalty from §2.2 L4. Pick the best 2 or 3 routes that share no relay as the lane set.
- **Drain:** loss > 1%, 30 s of missed heartbeats, or RTT drift > 2× baseline.
- **Relay hardening:**
  - XDP drop of unknown sources plus a per-source token bucket (start at about 1,500 pps / 3 Mbps per session; handshakes 10/s per IP).
  - Destination allowlist = ticket `dst_allow` ∩ catalog CIDRs. This extends today's `IsPublic` check in `internal/relay/policy.go`.
  - SSH only over WireGuard, immutable image, no payload logs.
- **Provisioning:** one Terraform module per provider (OCI, Vultr, Hetzner, Akamai, Gcore, GCP, Azure). Each outputs `{relay_id, ip, pop, provider}`; cloud-init installs `relay` and registers it with a one-time token.
- **Anycast:** later, own ASN + /24 via Vultr BGP ([docs](https://docs.vultr.com/configuring-bgp-on-vultr)), for control-plane and handshake ingress only. Game data stays on unicast relays chosen by measurement, as Valve SDR does ([Valve SDR](https://partner.steamgames.com/doc/features/multiplayer/steamdatagramrelay)).

**Metros.** Two providers per metro, with Valve's POP list ([GetSDRConfig](https://api.steampowered.com/ISteamApps/GetSDRConfig/v1/?appid=730)) as the map of where game servers are:

| Region | Metros | Providers (candidates) |
|---|---|---|
| GCC | Muscat (MC1 / Oman-IX, owned by Awasr ([sdxcentral](https://www.sdxcentral.com/news/awasr-ams-ix-and-alliance-networks-launch-internet-exchange-in-oman/), [PeeringDB](https://www.peeringdb.com/ix/4371))), Fujairah, Dubai, Riyadh/Jeddah/Dammam, Doha, Bahrain (AWS only, risky) | DataPacket Fujairah ([dc](https://www.datapacket.com/datacenters/fujairah)), Melbicom UAE ([vps](https://www.melbicom.net/virtualserver/uae/)), OCI `me-dubai-1`/`me-jeddah-1`/`me-riyadh-1`, Azure UAE North, Gcore Dubai, GCP `me-central2` |
| South Asia | Mumbai, Chennai | Vultr, Akamai, OCI, AWS ap-south-1 |
| EU | Frankfurt, Amsterdam, London, Paris, Madrid, Stockholm, Warsaw, Vienna, Helsinki, Marseille, Istanbul | Hetzner ([locations](https://docs.hetzner.com/cloud/general/locations/)), OVH, Vultr, DataPacket |
| NA | Ashburn, Chicago, Dallas, Atlanta, LA, Seattle | Vultr, Akamai, OVH, Hetzner |
| SA | São Paulo, Santiago, Buenos Aires, Lima | Vultr, Akamai |
| Asia/OCE | Singapore, Tokyo, Seoul, Hong Kong, Sydney | Vultr, Akamai, Hetzner `sin` |
| Africa | Johannesburg, Lagos, Nairobi | Vultr, Zenlayer |

### 3.5 Game and server catalog (`internal/catalog`, `catalog/` data repo)

```yaml
Game:       {id, names[], exe[{name, publisher, sha_prefix?}], launcher{steam, epic, riot, bnet, ea, ubi, xbox},
             anti_cheat: [vanguard|eac|battleye|faceit|ricochet|vac|none], allowed_backends[],
             shard_model: account|region|none, transport[{proto, ports}], p2p: bool,
             login_hosts[] (for Redirect login), discovery[]: [sdr|epic_icmp|gamelift|playfab|asn|static|learned],
             defaults{udp_lanes, tcp_lanes, dup}}
ServerPool: {pool_id "valorant:eu:riyadh", game_id, display "SA Riyadh", shard, city, country, lat, lon,
             provider, asn, prefixes[], beacon{type: icmp|udp_echo|sdr|gamelift|playfab|tcp_syn, host, port},
             status: active|degraded|offline, source, confidence, last_verified}
Observation:{game_id, dst/24, port, proto, asn, rtt p50/p95, jitter, loss, matched_pool?, client_asn, ts}
RelayPOP:   {id, city, lat, lon, provider, asn, ix[], measured_to{pool_id: rtt}}
```

**Sources, in order of trust:**
1. **Live APIs (hourly, ETag):**
   - Valve SDR per app id (730, 570, 1422450). Sanitize the swapped and wrong `geo` values.
   - Epic ping hosts ([Epic](https://www.epicgames.com/help/c-202300000001619/a202300000010042)).
   - GameLift `ListLocations` UDP beacons on 7770 ([AWS](https://docs.aws.amazon.com/gamelift/latest/developerguide/reference-udp-ping-beacons.html)).
   - PlayFab QoS UDP 3075 ([MS](https://learn.microsoft.com/en-us/rest/api/playfab/multiplayer/multiplayer-server/list-qos-servers-for-title)).
2. **Signed static catalog on a CDN**, weekly, carrying `status` so Bahrain can be marked offline without a client release.
3. **Monthly ASN and cloud ranges:**
   - RIPEstat `announced-prefixes` for AS6507 (Riot), AS32590 (Valve), AS57976 (Blizzard), AS22697 (Roblox, UNVERIFIED), plus EA, Ubisoft and Activision ASNs.
   - AWS `ip-ranges.json`, GCP `cloud.json`, Azure service tags.
4. **Flow learning:** the highest-pps UDP flow (TCP for WoW/PoE2) of the game process. Enrich it with ASN/prefix/geo and match a pool by prefix, or create a provisional `learned` pool "<ASN> <city>". Crowd promotion at ≥ 20 users.
5. **Speed-of-light sanity:** `rtt_min ≥ 2·distance / (200 km/ms)`. Any pool that violates it has wrong geo.

### 3.6 Measurement and diagnostics (`internal/diag`)

| Probe | API | Rate |
|---|---|---|
| Gateway | `IcmpSendEcho2` to the default gateway | 20 Hz during a session |
| Wi-Fi | `WlanQueryInterface(current_connection, rssi, realtime_connection_quality)`, `WlanGetNetworkBssList` (BSS Load, co-channel count) ([MS](https://learn.microsoft.com/en-us/windows/win32/api/wlanapi/ne-wlanapi-wlan_intf_opcode)) | 1 Hz |
| NIC throughput | `GetIfEntry2` In/OutOctets | 1 Hz |
| Bufferbloat | 5 s idle / 5 s down / 5 s up / 5 s recovery, 10 Hz ping to gateway + nearest relay; grade A+ < 5, A < 30, B < 60, C < 200, D < 400, F | On demand |
| MTR | `IcmpSendEcho2Ex` with TTL, plus UDP mode to the relay port, ≥ 200 samples/hop | On demand |
| TCP health | `SetPerTcpConnectionEStats` / `GetPerTcpConnectionEStats` ([MS](https://learn.microsoft.com/en-us/windows/win32/api/iphlpapi/nf-iphlpapi-getpertcpconnectionestats)) | 1 Hz for game TCP |
| Path | probe RTT, RFC 3550 jitter, TStats lanes | 10 Hz active, 1 Hz idle |

**Verdict engine** (first that fires wins, each with a confidence level):
1. **Wi-Fi:** gateway loss > 0.5% or p99 > 15 ms, plus RSSI < −70 dBm or 2.4 GHz.
2. **Overloaded line:** bufferbloat grade ≥ C, or the passive flag (relay RTT +20 ms while non-game throughput > 70% of line rate). Show the top process by bytes.
3. **ISP route unstable:** loss starts at an ISP hop and persists to the destination. Lowpath can help; show before/after.
4. **Game server lagging:** the route is clean up to the egress, egress→server is bad. Say it can't be fixed.
5. **Already optimal:** direct within 2 ms and no lossier. Recommend running without Lowpath.

Before/after is always the same method, the same target, 60 s each, with effective loss. Never show an unmeasured "estimated" ping as fact; label estimates "est." and show the measured figure next to them.

### 3.7 UI screens (`ui/src`)

1. **Onboarding:** language (Arabic/English, auto from the OS), location + ISP, library scan per launcher, 20 s connection check, consent toggles.
2. **Home:** detected games grid (Steam-style art), "Running now: Boost?" banner, last-session chip "−11 ms · 0.0% loss".
3. **Game page:**
   - Hero with anti-cheat badge and backend.
   - **Boost & Launch** (launcher URI).
   - Server picker grouped by region: direct ping, relay ping, loss, "Direct only" rows, **Auto (follow game)**.
   - Route diagram (§2.6 U2).
   - Re-optimize / Reset / Advanced (§2.6 U4).
4. **Live session** (also the tray flyout and mini window): big ping / jitter / loss with lane vs delivered loss, 5 min sparkline with switch markers, counters, Stop, Report a problem, session-end summary.
5. **Diagnostics:** traffic-light rows (Wi-Fi, bufferbloat, background downloads, DNS, MTU, IPv6, other VPNs/filters, power plan), segmented Home/ISP/Lowpath/Server bar, MTR view, export.
6. **Settings:** General, Overlay, Privacy, Advanced (capture backend, logs).
7. **Relays** (power users): self-hosted relays, `lowpath://relay?...` import, SSH deploy wizard, Test.
8. **Overlay:** click-through topmost window (`WS_EX_TOPMOST|LAYERED|TRANSPARENT|NOACTIVATE`); works over borderless and windowed games, not exclusive fullscreen. Phone view over LAN with a QR code.

---

## 4. Per-game table (top titles)

Legend:
- **Disc** (discovery method): SDR = Valve GetSDRConfig; EPIC = Epic ping hosts; GL = GameLift beacons; PF = PlayFab QoS; ASN = prefix lists; FL = flow learning.
- **ICMP:** whether servers answer ping (U = unverified).
- **AC risk:** anti-cheat risk for the WinDivert backend; `tun` mode is lower for all of them (UNVERIFIED, lab must confirm).

| Game | Server/region source | Transport (ports) | Disc | ICMP | Anti-cheat / risk |
|---|---|---|---|---|---|
| VALORANT | [Riot server select](https://support.riotgames.com/en-us/valorant/support-tools/server-select) (1 Jun 2026): NA 6 sites, LATAM Santiago/Mexico City/Miami, BR São Paulo, EU Frankfurt/Paris/Stockholm/Istanbul/London/Warsaw/Madrid/Bahrain, KR Seoul, AP Hong Kong/Tokyo/Singapore/Sydney/Mumbai/Manila. The Riyadh/Mumbai ME fallback the user sees is **not** on that page (Sec) | UDP 7000–8000, 8180–8181; TCP 2099, 5222–5223, 8088, 8393–8400, 443 ([Riot](https://support.riotgames.com/en-us/valorant/support-tools/how-to-set-up-port-forwarding)) | ASN AS6507 ([PeeringDB](https://www.peeringdb.com/api/net?asn=6507)) + FL | U | Vanguard (kernel, boot): **High** for divert; `tun` only |
| League of Legends / TFT | ME1 since Jun 2024 ([Riot](https://support-teamfighttactics.riotgames.com/hc/articles/29268172644499)); EUW/EUNE | UDP 5000–5500; TCP 2099, 5222–5223, 8393–8400 | ASN AS6507 + FL | U | Vanguard: **High**; TCP lanes matter |
| CS2 | SDR app 730: dxb, dfjr, bom2, fra… | UDP via SDR 27015–27060 | SDR | Yes (relays) | VAC user mode: low. FACEIT: **High** |
| Dota 2 | SDR app 570 | UDP SDR | SDR | Yes | VAC: low |
| Deadlock | SDR app 1422450 (SDR use UNVERIFIED) | UDP SDR | SDR | Yes | VAC: low |
| Fortnite | Epic ping hosts (ping-bah, ping-mum, ping-eu…) | UDP ~7777–9000 (U) | EPIC + FL | Yes (ping hosts) | EAC: **High** (2026 WinDivert reports) |
| Apex Legends | In-game DC list; Bahrain offline since Mar 2026 (Sec) | UDP 37000–40000 (U) | FL (+GL if AWS) | U | EAC: **High** |
| PUBG | Manual region; ME on AWS me-south-1 (Sec) | UDP 7080–8000 (U) | GL proxy + FL | U | BattlEye: Medium-High |
| Rainbow Six Siege | Azure/PlayFab, `uaenorth` hint ([Steam](https://steamcommunity.com/app/359550/discussions/1/3201493834353217375)) | UDP | PF + FL | U | BattlEye: Medium-High |
| Overwatch 2 | Hidden ME sub-region (Sec) | TCP 1119/3724/6113; UDP 26503 (U) | ASN AS57976 + FL | U | Blizzard AC: Medium |
| CoD Warzone / BO7 | Demonware multi-cloud (Sec) | UDP 3074+ | FL | U | Ricochet (TPM + Secure Boot) ([TechRadar](https://www.techradar.com/gaming/pc-gaming/activisions-call-of-duty-black-ops-7-also-has-tpm-2-0-and-secure-boot-requirements-and-kernel-level-anti-cheat-shows-no-signs-of-slowing-down)): **High** |
| Rocket League | ME, ASM (Mumbai) regions (Sec) | UDP 7000–9000 (U) | FL | U | EAC: Medium-High |
| Marvel Rivals | ME = Dammam (inference GCP me-central2) ([fan list](https://marvelrivals.gg/servers/)) | UDP | FL + GCP ranges | U | NetEase AC: Medium (U) |
| EA FC 26 | Dubai, Manama in fan DC list ([fifauteam](https://fifauteam.com/fc-26-servers/)) | UDP 3659 (U) | FL | U | EA Javelin: Medium (U) |
| Roblox | NA/EU/Asia, no confirmed ME | UDP 49152–65535 (Sec) | ASN AS22697 (U) + FL | U | Hyperion: Medium (U) |
| Escape from Tarkov | Launcher list incl. Dubai (Sec) | UDP 17000–17100 (U) + TCP | FL | Often (U) | BattlEye: Medium-High |
| Destiny 2 | P2P + Bungie hosts | UDP 3074, 3097–3196 ([Bungie](https://help.bungie.net/hc/articles/360049496751)) | FL (peers) | Peers maybe | BattlEye: Medium-High |
| Lost Ark | NA/EU/SA, AWS | TCP/UDP 6040 (U) | GL + FL | U | EAC: Medium-High |
| Path of Exile 2 | Gateways at login | **TCP** 6112, 20481 (Sec) | FL | U | None known: low |
| World of Warcraft | Realms EU/NA | **TCP** 3724, 1119 | ASN AS57976 + FL | U | Warden: low-medium |
| Street Fighter 6 / Tekken 8 | P2P rollback | UDP, random ports | FL (peer IPs, per match) | Peer-dependent | EAC (Tekken 8, U): Medium; relay helps only the peer path |
| Minecraft | User-entered server | Java TCP 25565; Bedrock UDP 19132 | User address | Varies | None: low |
| Genshin Impact | Account servers (Sec) | UDP/KCP 22101–22102 (U) | FL | U | mhyprot (kernel): Medium (U) |
| Mobile Legends (emulator) | No authoritative ME DC | UDP | FL (emulator process) | U | Low (U) |

Every title also has NA, SA, Asia and OCE pools; the same discovery method applies in every region. P2P titles get "relay the peer path" mode: both ends benefit only when the peer's ISP path is the bad one, so be honest in the UI.

**Honest expectation for Valve SDR titles (CS2, Dota 2, Deadlock).** Valve already relays these games over its own backbone, with POPs in Dubai (`dxb`) and Fujairah (`dfjr`) ([GetSDRConfig](https://api.steampowered.com/ISteamApps/GetSDRConfig/v1/?appid=730)). Lowpath can only improve the first mile to the nearest Valve POP, plus loss through duplication. From Muscat the direct path to `dxb` is already 17 ms, so expect a gain of a few ms at most and say that in the UI (inference; confirm in M2).

### 4.1 Further titles (catalog batch 2, M5)

Mostly UNVERIFIED (from memory or secondary sources) unless a link is given. Each entry ships only after a lab capture confirms ports and process names.

| Game | Server model | Transport (ports) | Disc | Anti-cheat / risk |
|---|---|---|---|---|
| Battlefield 6 | EA dedicated, multi-region | UDP (U) | FL | Javelin kernel; **Secure Boot + TPM 2.0 required** ([EA](https://www.ea.com/games/battlefield/battlefield-6/news/secure-boot-information)): **High** |
| ARC Raiders | Embark dedicated; regions not officially published | UDP (U) | FL | EAC ([NoPing guide](https://noping.com/blog/arc-raiders-beginner-guide-2026-us-servers-ping-latency-and-connection-optimization), Sec): **High** |
| Dead by Daylight | Dedicated, auto region (U) | UDP (U) | FL (+GL, U) | EAC: **High** |
| Rust | Official + community servers; the IP is in the server browser | UDP 28015 default (U) | Server IP + N10 | EAC: **High** |
| DayZ / Arma Reforger | Community servers | UDP 2302+ (U) | Server IP + N10 | BattlEye: Medium-High |
| GTA Online / FiveM | P2P sessions with Rockstar services; FiveM community servers on 30120 (U) | UDP/TCP | FL (peers) / N10 | BattlEye (GTA Online, U): Medium-High |
| Hunt: Showdown 1896, The Finals | Dedicated, regional | UDP (U) | FL | EAC: **High** |
| Delta Force | Tencent dedicated | UDP (U) | FL | ACE kernel (U): **High** |
| Naraka: Bladepoint | NetEase dedicated | UDP (U) | FL | NetEase AC (U): Medium |
| Halo Infinite, Sea of Thieves, Age of Empires IV | Azure / PlayFab (U) | UDP | PF + FL | Medium (U) |
| Warframe, Helldivers 2, Monster Hunter Wilds, Elden Ring Nightreign | P2P host or P2P sessions (U) | UDP | FL (peers) | EAC / GameGuard (U): Medium-High; peer-path mode only |
| Final Fantasy XIV | Data centers NA/EU/JP/OCE | **TCP** 54992–54994, 55006–55007, 55021–55040 (U) | FL | Low |
| Black Desert, Throne and Liberty, New World, Albion Online, MapleStory | MMO regions; Throne and Liberty and New World on AWS (U) | Mostly TCP or mixed (U) | FL (+GL) | Varies (U) |
| Hearthstone, StarCraft II, Diablo IV | Blizzard AS57976 | TCP 1119/3724 (+UDP for Diablo IV, U) | ASN + FL | Warden: low-medium |
| 2XKO, Teamfight Tactics | Riot AS6507 (U) | UDP | ASN + FL | Vanguard (U): **High** |
| Wuthering Waves, Zenless Zone Zero, Honkai: Star Rail | Account-bound regional servers (U) | UDP/KCP (U) | FL | Kernel AC (U): Medium |
| Fall Guys | Epic (U) | UDP | EPIC + FL | EAC: **High** |
| PUBG Mobile, Free Fire, CoD Mobile, MLBB, Honor of Kings (emulator) | Tencent / Garena / Activision / Moonton cloud (U) | UDP | FL inside emulator traffic (G7) | Emulator detection varies (U) |

### 4.2 Region playbook (every player region, not just the Gulf)

A region counts as **done** when: at least 2 beta testers on 2 different ISPs each submit a same-method before/after report for 3 games; the region has relays from 2 providers; and the verdict engine has been checked against a known-bad Wi-Fi case. Server locations come from §4 and the Valve POP list; the "typical problem" column is a field hypothesis to confirm with telemetry (UNVERIFIED).

| Region | Where servers sit | Typical problem | Ingress → egress plan | Notes |
|---|---|---|---|---|
| GCC (OM, AE, SA, QA, KW, BH) | Dubai, Fujairah (Valve), Riyadh, Dammam (GCP), Bahrain (AWS, damaged 2026), Mumbai, Frankfurt | ISP transit detours; single-country cloud outages | In-country ingress (Oman-IX/MC1, UAE-IX, Riyadh) → per-game egress | Legal flags per country (R1) |
| Levant, Iraq, Egypt | Istanbul, Frankfurt, Dubai, Bahrain | Lossy last mile, few local peering points | Istanbul / Dubai ingress; Egypt possibly via Marseille cable landings | Loss is often local; verdict engine first |
| North Africa (MA, DZ, TN) | Paris, Madrid, Frankfurt | Long detours through France | Madrid / Marseille ingress → EU egress | French UI later |
| Sub-Saharan Africa (ZA, NG, KE) | Johannesburg (Valve `jnb`), Cape Town (some games, Sec), otherwise EU | Few local servers; trans-ocean paths | Johannesburg, Lagos, Nairobi (Zenlayer) ingress → EU egress | Cost per GB is high on African providers (UNVERIFIED) |
| South Asia (IN, PK, BD, LK) | Mumbai, Chennai (Valve `maa2`), Singapore, Dubai | Peak-hour loss; some ISPs reach Mumbai via Singapore | Mumbai / Chennai / Delhi ingress | Huge mobile-on-emulator share (G7) |
| SEA (PH, ID, VN, TH, MY, SG) | Singapore, Hong Kong, Manila (VALORANT), Tokyo | Island ISPs route via HK or the US | Manila, Jakarta, Ho Chi Minh City, Bangkok ingress → SG/HK egress | Indonesian, Filipino UI (U8) |
| East Asia (JP, KR, TW, HK) | Tokyo, Seoul, Hong Kong | Mostly fine at home; demand is cross-region play (G10) | Tokyo / Seoul egress for long-haul players | **Mainland China is out of scope** (licensing and legal) |
| Oceania (AU, NZ) | Sydney; others far | Distance; NZ depends on trans-Tasman paths | Auckland, Melbourne, Perth ingress → Sydney | |
| North America | Ashburn, Chicago, Dallas, Atlanta, LA, Seattle, Oregon | Evening peering congestion | Valve POP metros, 2 providers each | Largest competitor presence |
| Latin America (MX, CO, PE, CL, AR) | Miami, Santiago, Mexico City, São Paulo | Traffic hairpins through Miami | Bogotá, Lima, Buenos Aires, Mexico City ingress | es-419 UI (U8) |
| Brazil | São Paulo | North and Northeast are far from São Paulo; ISP peering varies | Fortaleza (cable landing, U), Rio, Porto Alegre ingress → São Paulo | ExitLag's home market; strongest competition |
| EU West and Central | Frankfurt, Amsterdam, London, Paris, Madrid, Stockholm, Warsaw, Vienna | Usually small gains; Wi-Fi and bufferbloat dominate | Few relays; diagnostics are the product | Honest "Already optimal" will fire often |
| EU East, Turkey, CIS | Warsaw, Istanbul, Stockholm, some Moscow | Publishers withdrew from Russia; blocking of game servers reported ([NoPing](https://noping.com/blog/rkn-arc-raiders-rossiya-obhod-blokirovok), Sec) | Istanbul / Warsaw | **No paid service in Russia** until a legal review (R1); Turkish UI (U8) |

---

## 5. Phased backlog

Each milestone ends with a field test on a real gaming PC (the reference setup is a player in the Gulf). From M0 on, every test must run **without anyone clicking a UAC prompt** (tests are often driven remotely). File paths are targets.

### Parallel track: long-lead [USER] items (start in week 1, not when needed)

| Item | Lead time (estimate) | Needed by | Done when |
|---|---|---|---|
| Apply to SignPath Foundation (free OSS signing); fall back to an OV cert quote | 2 to 6 weeks | M7 | A signed test build passes `signtool verify /pa` |
| OCI Always Free tenancy in `me-dubai-1` or `me-jeddah-1` (home region is fixed at signup) | 1 day, plus capacity retries | M2 | An A1 relay answers `lpcheck` |
| Company registration (Omani CR) and D-U-N-S, only if the driver track (M8) goes ahead | 4 to 12 weeks | M8 | CR certificate in hand |
| Legal memo on tunnelling game traffic in OM, AE, SA (R1) | 2 to 4 weeks | M6 | Memo filed in `docs/legal/` |
| Secondary game accounts for the anti-cheat lab (VALORANT, Fortnite, Apex, R6, PUBG, CS2+FACEIT, BO7, BF6) | 1 week | M3 | Accounts listed in `docs/AC-LAB.md` |

### M0: "Testable remotely, safe to fail" — 1 week

| Task | Size | Where | Accept |
|---|---|---|---|
| Dev-mode `LowpathSvc`: install once with one UAC, then the UI talks to it over the named pipe (the M7 split, minimal version) | M | `cmd/lowpathsvc`, `internal/app` | Reboot, then Boost from the UI with no prompt |
| Fail-open watchdog and boot-time cleanup (C5) | S | `internal/capture`, `cmd/lowpathsvc` | `taskkill /F` mid-session restores direct traffic within 1 s |
| Engine latency benchmarks in CI (L12) | S | `internal/engine/bench_test.go` | Budget numbers in the CI log; a regression fails the build |
| Conflict detector for other boosters, VPNs and WinpkFilter (C7) | S | `internal/compat/conflicts_windows.go` | With ExitLag installed, the UI names it |

**Field test (remote):** reboot the PC, start a session from the remote UI, kill the service mid-session and confirm the game keeps playing.

### M1: "Know where the lag is" (no servers, no money) — 3 to 4 weeks

| Task | Size | Where |
|---|---|---|
| Gateway 20 Hz pinger + segmented Home/ISP/Server view | M | `internal/diag/gateway.go`, `ui/src/components/Diagnostics.tsx` |
| WLAN API: RSSI, band, Tx rate, BSS Load, background-scan/streaming-mode toggle with revert | M | `internal/diag/wlan_windows.go` |
| Bufferbloat test + passive detector (`GetIfEntry2`) | M | `internal/diag/bloat.go` |
| MTR (`IcmpSendEcho2Ex` TTL) + UDP mode + persistence rule | M | `internal/icmp/ttl_windows.go`, `internal/diag/mtr.go` |
| Verdict engine + exportable report | S | `internal/diag/verdict.go` |
| Location/ISP: public IP via STUN (reuse `cmd/lpcheck/stun.go`) + ASN lookup (RIPEstat `network-info`, cached) | S | `internal/diag/whoami.go` |
| Catalog v1: Valve SDR live fetch (730/570/1422450), sanitize geo, show POPs with direct ping on the game page | M | `internal/catalog/sdr.go`, `GamePanel.tsx` |
| Epic ping hosts + GameLift beacon probes as pools | S | `internal/catalog/epic.go`, `gamelift.go` |
| Same-method before/after over 60 s, "Already optimal" verdict | S | `internal/engine/baseline.go` |
| Engine: 250 ms late-copy bound, per-path late/too-old counters | S | `internal/dedup`, `internal/engine` |
| "Current server" card for **every** game: the flow-learned server with ASN, city (geo + speed-of-light check) and live ping/jitter/loss, even with no catalog entry. For anti-cheat titles, learn flows **without loading WinDivert**: ETW `Microsoft-Windows-Kernel-Network` gives per-PID UDP/TCP send/receive events with remote address and port (field names to confirm in the lab) | M | `internal/engine/flows.go`, `internal/detect/etw_windows.go`, `GamePanel.tsx` |
| Upload vs download loss split (L11, client side from the dedup window) | S | `internal/engine` |

**Field test:** open Diagnostics on Wi-Fi and then Ethernet and compare the verdicts. Start a Steam download and watch the bufferbloat grade. CS2 game page lists `dxb ≈ 17 ms`, `bom2`, `fra ≈ 113 ms` before launch. Fortnite lists ping-bah, ping-mum and ping-eu. VALORANT, LoL and Rocket League (no public beacon) show the server they actually connected to, with a city and ASN, after one match.

### M2: "First real relays in the Gulf" — 2 to 3 weeks

| Task | Size | Who |
|---|---|---|
| Create OCI Always Free tenancy, home region `me-dubai-1` (or `me-jeddah-1` for Riyadh) ([OCI free tier](https://docs.oracle.com/en-us/iaas/Content/FreeTier/resourceref.htm)) | S | **[USER]** account + card for verification |
| Rent Melbicom Fujairah KVM (~€4–6/mo) + one Mumbai and one Frankfurt VPS | S | **[USER]** money (~$30/mo) |
| `deploy-relay.ps1`: arm64 path, OCI security-list hint, BBR/fq sysctls | S | `deploy/` |
| Relay STUN echo, so the client learns its public IP from any relay | S | `internal/relay` |
| Route diagram per hop with stacked lanes; Re-optimize / Reset / Advanced on the game page | M | `RouteMap.tsx`, `GamePanel.tsx` |
| "Use local relays first" (country/ASN of the relay vs the client) | S | `internal/engine` |
| Forwarder chain measurement (per-leg ms reported by hops) | M | `internal/relay/forward.go`, `internal/proto` |
| MTR + traceroute from the user's line to each relay, saved as a report | S | uses M1 |

**Field test** (remote, through the M0 service): boost CS2 via Fujairah → Dubai vs direct, 60 s each, and record CS2's own telemetry HUD next to Lowpath's figures (L10). Then boost VALORANT via the Jeddah/Dubai relay toward Riyadh **on a secondary account only**, until M3 (Vanguard risk with WinDivert; a custom game does not lower ban risk because Vanguard runs either way). Pass: relayed is ≥ 3 ms better or has lower loss, and the game's own meter agrees. If not, the verdict says so honestly.

### M3: "Anti-cheat-safe capture + TCP" — 5 to 7 weeks

| Task | Size | Where |
|---|---|---|
| `CaptureBackend` interface; move WinDivert code behind it | M | `internal/capture/backend.go` |
| `tun` backend: WinTUN adapter, `/32` and prefix routes, PID filter, re-send of non-game packets on the physical NIC | L | `internal/capture/tun_windows.go` |
| gVisor netstack TCP termination → `TStream` to egress; egress dials the real server; BBR/fq on relays | L | `internal/tcptun`, `internal/relay/stream.go` |
| Redirect login: catalog `login_hosts` + Riot/Blizzard TCP ports pinned to the UDP egress | M | `internal/catalog`, `internal/engine` |
| ICMP relay in tun mode (`TPing`) | S | `internal/proto`, `internal/relay` |
| Compat gate: detect `vgk`, `EasyAntiCheat`, `BEService`, `FACEIT` services; per-game allowed backends; auto-fallback | M | `internal/compat` |
| PMTU probe, Lowpath fragmentation, MSS clamp | M | `internal/engine/mtu.go` |
| Anti-cheat lab runbook: VALORANT, LoL, Fortnite, Apex, R6, PUBG, CS2+FACEIT, BO7, Battlefield 6, ARC Raiders; 20 matches over 14 days, 30-day ban follow-up, re-run within 7 days of every anti-cheat update (C2) | M | `docs/AC-LAB.md` + **[USER]** secondary accounts |
| Boost-while-running behaviour and TCP notice (C6) | S | `internal/engine`, `GamePanel.tsx` |
| ICMP lanes in Advanced (C4) | S | `internal/engine`, `GamePanel.tsx` |

**Field test:** VALORANT unrated match on the secondary account in `tun` mode via Dubai relay → Riyadh, Vanguard running, no tray notice, account still clean after 30 days. LoL login + match with TCP lanes = 1. PoE2 (TCP) through Frankfurt. Start VALORANT first and Boost second: no disconnect.

### M4: "Loss and jitter engine v2" — 4 to 5 weeks

| Task | Size | Where |
|---|---|---|
| Protocol v3: `path_id`, `copy`, `frag`, AEAD (ChaCha20-Poly1305), `THello` ticket (self-signed for BYO relays) | L | `internal/proto` (update architecture.md contract) |
| Egress per-path bitmap → `TStats` (`p_A, p_B, p_AB, ρ`, Gilbert p01/p10, RFC 3550 jitter) | M | `internal/relay/egress.go` |
| Time-shifted duplicates with `CreateWaitableTimerExW(HIGH_RESOLUTION)` | M | `internal/engine/shift_windows.go` |
| Redundancy policy table + hysteresis + disjointness scoring | M | `internal/engine/policy.go` |
| New cost function, replacing `Stats.Score` | S | `internal/engine/tracker.go` |
| DSCP via `QOSSetFlow` toggle; Delivery Optimization throttle toggle (with revert) | S | `internal/diag/qos_windows.go` |
| Optional RS FEC for TCP streams / > 200 pps | M | `internal/fec` |
| Live screen: lane vs delivered loss, switch markers, session summary | M | `ui/src/components/LiveGraph.tsx` |
| netsim: Gilbert-Elliott bursts and a shared-segment topology | S | `internal/netsim` |
| Per-direction redundancy policy (L11, egress side) | S | `internal/relay/egress.go`, `internal/engine/policy.go` |
| **Multi-Internet** (moved up from M9): lanes bound to Ethernet + Wi-Fi + USB phone tether with `IP_UNICAST_IF`; metered-link rule (N9) | L | `internal/engine/path.go`, `internal/diag/links_windows.go` |

**Field test:** on Wi-Fi, a 30-minute match with two lanes (Fujairah + Dubai). Live screen shows lane loss vs delivered loss, split by direction; the gateway row shows whether Wi-Fi caused it. Microwave/2.4 GHz experiment: time-shift turns on by itself. Then plug in a USB-tethered phone and pull the Ethernet cable mid-match: no stall over 100 ms, and phone data stays under 5 MB per clean hour.

### M5: "Every game, every region (catalog)" — 5 to 6 weeks

| Task | Size | Where |
|---|---|---|
| Catalog schema + signed JSON (Ed25519), CDN publish, client cache with `status` | M | `internal/catalog`, `catalog/` repo |
| Monthly jobs: RIPEstat prefixes for the ASNs above, AWS/GCP/Azure ranges | M | `cmd/catalogd` |
| Pools for the 24 titles in §4, all regions (batch 1), then the §4.1 titles (batch 2) | L | data work |
| Emulator detection and per-game flow classification for mobile titles (G7) | M | `internal/detect`, `internal/engine/flows.go` |
| Catalog freshness CI and "no game flow found" alert (G8) | S | `cmd/catalogd` |
| Community servers, nearest egress to destination (N10), first version: pick the relay closest to the destination's geo and check it with an egress probe. The mesh-measured version comes with the registry in M6 | M | `internal/engine` |
| Every region the game offers in the server picker, with no geo-unblocking (G10) | S | `GamePanel.tsx` |
| Optional Voice profile for Discord (G9) | S | `internal/catalog` |
| Flow-learning classifier → provisional pools; "label this server" prompt | M | `internal/engine/flows.go`, `internal/catalog/learn.go` |
| Auto (follow game): detect the match server, switch egress, pre-warm 2 candidate egresses | L | `internal/engine` |
| Region failover from catalog `status` | S | `internal/engine` |
| Launcher library: Steam ACF, Epic `.item`, Riot YAML, Battle.net, Ubisoft registry, Xbox `.GamingRoot`; Boost & Launch URIs | M | `internal/detect` |
| Process identity by path + Authenticode signer | S | `internal/detect` |

**Field test:** Fortnite, Apex, R6, Marvel Rivals and Rocket League each show a server list with live pings before launch. Change the Fortnite region in game and the route follows. PUBG Mobile in BlueStacks is detected and routed. A Rust community server in Frankfurt gets an FRA egress.

### M6: "Community relay network" — 6 to 8 weeks

| Task | Size | Who / where |
|---|---|---|
| Registry service (register, heartbeat, relaylist, sessions/tickets, mesh, telemetry) | L | `cmd/registry`, `internal/registry` |
| Relay: tickets, destination allowlist, cookie challenge, heartbeat | M | `internal/relay` |
| XDP filter + per-source token bucket | M | `deploy/xdp/` |
| Terraform modules per provider + cloud-init bootstrap | M | `deploy/terraform/` + **[USER]** provider accounts |
| Mesh probing + route-cost solver | M | `internal/registry/route.go` |
| Opt-in telemetry pipeline, per-ISP "local routes" promotion | M | `internal/registry/telemetry.go` |
| GCC expansion: DataPacket Fujairah quote, Oman-IX/MC1 peering or colo request to Awasr IGW (AS214982, open policy ([PeeringDB](https://www.peeringdb.com/net/36226))) | — | **[USER]** money + business contact |
| Public relay directory: anyone can run `relay` and publish it (signed listing, health-checked by the registry); target two providers in each metro of §3.4 | M | `internal/registry`, `docs/relays/` + community hosts |
| Mesh-measured nearest egress for community servers (N10, full version) | S | `internal/registry/route.go` |
| Region rollout by the §4.2 playbook: recruit 2 testers on 2 ISPs per region, collect same-method reports, mark the region done | M per region | **[USER]** community + `docs/regions/` |
| Legal memo applied: per-country policy flag in the catalog, marketing check list (R1) | S | `internal/catalog`, `docs/legal/` |

**Field test:** with the registry live, kill the Dubai relay mid-match: the client re-routes in < 2 s and shows a toast. A friend in Riyadh or Mumbai installs and gets local ingress automatically. Outside the Gulf, at least one region from each of South Asia, SEA, EU and LATAM reaches "done" by the §4.2 rule.

### M7: "Real product shell" — 5 to 6 weeks

| Task | Size | Who / where |
|---|---|---|
| Harden the M0 dev service into the production `LowpathSvc` + unelevated UI over a named pipe | M | `cmd/lowpathsvc`, `cmd/lowpath`, `internal/app` |
| Tray (`Shell_NotifyIconW`), AUMID toasts, HKCU Run autostart | M | `cmd/lowpath/tray_windows.go` |
| Inno Setup installer (service, shortcut, clean uninstall incl. driver/adapter) | M | `installer/lowpath.iss` |
| Signed update manifest + `WinVerifyTrust` + staged rollout/rollback | M | `internal/updater` |
| Code signing (I1): SignPath Foundation if accepted (applied in the parallel track), otherwise an OV cert on a cloud HSM (SSL.com eSigner / DigiCert KeyLocker; Oman eligibility UNVERIFIED). Azure Artifact Signing is not open to Oman entities ([MS](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options)). Submit the signed installer to the Microsoft Store as a second channel | S | **[USER]** $0 or $150–300/yr, identity verification |
| Sentry Go + WER LocalDumps, opt-in | S | `internal/crash` |
| Arabic/RTL pass, ICU plurals, bundled font | M | `ui/src` |
| Overlay window + phone view | M | `cmd/lowpath/overlay_windows.go` |
| Traffic shaper (per-app caps) | M | `internal/shaper` |
| Session history and best-server hint (U9); data-use display and metered warning (U10) | M | `internal/store`, `ui/src` |
| Request a game + support bundle (U11, D6) | S | `ui/src`, registry |
| String tables for pt-BR, es-419, tr, ru, id, fil, hi (U8) | M | `ui/src/i18n` |
| "PC as gateway" for consoles and phones on the Windows hotspot / ICS (U12 step 1) | M | `internal/capture` (source-subnet target) |

**Field test:** run the installer (one UAC), reboot, and the tray appears with no UAC. Boost from the tray; get an update toast between matches; uninstall leaves nothing behind. Switch the UI to Portuguese and Turkish and check every screen. A phone or console joined to the PC hotspot shows up as a device and is routed.

### M8: "Own WFP driver" — 8 to 12 weeks, needs a company

| Task | Size | Who |
|---|---|---|
| Register a company (e.g. Omani CR), D-U-N-S if possible | — | **[USER]** |
| EV code-signing cert on a token/HSM; Partner Center (Hardware Developer Program) + Entra tenant; identity checks ([MS register](https://learn.microsoft.com/en-us/windows-hardware/drivers/dashboard/hardware-program-register)) | — | **[USER]** ~$300–700/yr |
| `lowpath.sys` KMDF + WFP callout (ALE connect/bind redirect), HVCI-compliant ([MS](https://learn.microsoft.com/en-ca/windows-hardware/drivers/driversecurity/implement-hvci-compatible-code)), IOCTL open only to the service SID | L | driver engineer |
| Driver Verifier, HLK HVCI readiness test, IOCTL fuzzing | M | |
| Attestation signing ([MS](https://learn.microsoft.com/en-us/windows-hardware/drivers/dashboard/code-signing-attestation)) → later WHCP/HLK | M | **[USER]** submission |
| Disclosure email to Riot, Epic/EAC, BattlEye, FACEIT with signer + hash (no known allow-list program) | S | **[USER]** |

**Field test:** `wfp` backend boosts VALORANT and Fortnite with HVCI + Secure Boot on; `sc query` shows no WinDivert or WinTUN in use.

### M9: Later (each item has its own pass test)

| Item | Accept |
|---|---|
| Mobile apps (Android `VpnService`, iOS `NEPacketTunnelProvider`) with Wi-Fi + cellular duplication | PUBG Mobile on Android over Wi-Fi with induced 3% loss: delivered loss < 0.3%; cellular data per clean hour shown and < 10 MB |
| Router package (OpenWrt) and console mode beyond the PC gateway (U12 step 2) | A PS5 behind an OpenWrt router plays through a relay with no PC running |
| FPS-boost style tweaks, only reversible ones with evidence: WLAN scan, power plan. No snake oil like `timeBeginPeriod` or `NetworkThrottlingIndex` ([MS](https://learn.microsoft.com/en-us/windows/win32/api/timeapi/nf-timeapi-timebeginperiod)) | Every tweak is listed in a "changes made" panel and fully reverted by one button (registry diff before vs after is empty) |
| In-game overlay tier 2 (Xbox Game Bar widget) | The widget shows live ping/loss in exclusive-fullscreen games where the tier-1 window cannot draw |

---

## 6. What cannot be done, or needs outside help

1. **Relays cost real money, so the network grows from self-hosted and community relays.**
   - About 27 GB of relay egress per user per month (2 copies, 2 hops, 60 h).
   - Fleet: about $100/mo for the Gulf phase, about $1,000 to $1,500/mo for 35 metros × 2 providers.
   - AWS Global Accelerator in the ME is about 10× the cost ([pricing](https://aws.amazon.com/global-accelerator/pricing/)); excluded.
2. **The "9 ms to Dubai" outcome depends on peering we don't control.** Riot Direct (AS6507) has no exchange port in Oman or Bahrain ([PeeringDB](https://www.peeringdb.com/api/net?asn=6507)). An Oman ingress helps only if the local ISP's path from MC1 to UAE-IX beats the default; that is measurable in M2, not promisable. An MC1 cross-connect needs a company and a colo contract.
3. **The anti-cheat outcome cannot be guaranteed.** No vendor publishes an allow-list. Even a signed WFP driver is only *lower* risk (UNVERIFIED). Ranked play on a valuable account is the user's risk until the lab has months of clean results.
4. **Driver signing needs a legal entity, an EV certificate, Microsoft identity checks and weeks of lead time.** Attestation signing is now described by Microsoft as for testing, so WHCP/HLK is the long-term path ([MS](https://learn.microsoft.com/en-us/windows-hardware/drivers/dashboard/driver-signing-offerings)).
5. **SmartScreen reputation can't be bought.** EV no longer gives instant reputation ([MS](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation)). Expect weeks of warnings even when signed.
6. **No fix for home-side loss.** If the loss is on the user's Wi-Fi or access line, relays can't remove it with one uplink. The product says so and offers Ethernet, 5/6 GHz, SQM on the router, or Multi-Internet (M4).
7. **P2P games** (SF6, Tekken 8, Destiny fireteams) only improve when the bad segment is on our side of the peer path.
8. **Server lists for Riot, CoD, EA, Apex and Roblox are not published.** They come from ASN data + flow learning + crowd data. Expect the first weeks in a new region to show "learned" labels.
9. **Tunnelling is legally sensitive in the Gulf and elsewhere.** UAE cybercrime law penalizes using another IP to commit or hide a crime, including reaching blocked gaming platforms ([Khaleej Times](https://www.khaleejtimes.com/business/tech/vpn-uae-6-million-downloads-dh2-million-fine-misuse?amp=1)); Oman's TRA warns about unauthorized VPN use ([MEA Tech Watch](https://meatechwatch.com/?p=55356)). Lowpath stays a game-traffic optimizer with a destination allowlist and never sells region unlocking. A local lawyer, not this plan, has to sign that off (R1).
10. **Valve SDR games gain little.** CS2, Dota 2 and Deadlock already ride Valve's own relays, including Dubai and Fujairah. Expect a few ms at most, mostly from loss protection (§4).
11. **Some regions are out of scope:** mainland China (licensing and filtering), sanctioned countries, and public relays in Russia until reviewed.
12. **Official server lists go stale.** Riot's own page still listed Bahrain and no Riyadh in Oct 2026, while the client showed Riyadh. What the game connects to always wins over any published list.

---

## 7. Key sources

- **Valve:** [GetSDRConfig](https://api.steampowered.com/ISteamApps/GetSDRConfig/v1/?appid=730), [SDR docs](https://partner.steamgames.com/doc/features/multiplayer/steamdatagramrelay)
- **Riot:** [server select](https://support.riotgames.com/en-us/valorant/support-tools/server-select), [ports](https://support.riotgames.com/en-us/valorant/support-tools/how-to-set-up-port-forwarding), [Riot Direct](https://technology.riotgames.com/node/25), [PeeringDB AS6507](https://www.peeringdb.com/api/net?asn=6507), [Vanguard](https://support.riotgames.com/en-us/riot/performance/what-is-vanguard)
- **Other discovery:** [Epic ping hosts](https://www.epicgames.com/help/c-202300000001619/a202300000010042), [GameLift beacons](https://docs.aws.amazon.com/gamelift/latest/developerguide/reference-udp-ping-beacons.html), [PlayFab QoS](https://learn.microsoft.com/en-us/rest/api/playfab/multiplayer/multiplayer-server/list-qos-servers-for-title)
- **Gulf outage:** [DCD](https://www.datacenterdynamics.com/en/news/aws-uae-outage-after-objects-struck-the-data-center-cause-fire-amid-iran-attacks/), [w.media](https://w.media/aws-cant-recover-resources-and-data-stored-in-uae-region-hit-during-us-iran-conflict/)
- **Competitors:**
  - ExitLag: [routes KB](https://www.exitlag.com/knowledgebase/101), [multi-internet](https://www.exitlag.com/knowledgebase/203)
  - NoPing: [multi-connection](https://noping.com/features/multi-connection)
  - GearUP: [AIR](https://www.gearupbooster.com/en/support/what-is-adaptive-intelligent-routing-technology.html)
  - Mudfish: [multipath](https://docs.mudfish.net/en/docs/mudfish-features/multipath-node-mode), [WFP mode](https://docs.mudfish.net/en/docs/mudfish-features/wfp-item-mode/)
  - [WinpkFilter use (ntkernel)](https://ntkernel.com/?p=12768)
- **Capture and anti-cheat:** [WinDivert releases](https://github.com/basil00/WinDivert/releases), [EAC vs WinDivert 2026](https://github.com/Flowseal/zapret-discord-youtube/discussions/16128), [WFP redirect](https://learn.microsoft.com/nb-no/windows-hardware/drivers/network/using-bind-or-connect-redirection), [Mullvad split tunnel](https://github.com/mullvad/win-split-tunnel), [driver blocklist KB5020779](https://support.microsoft.com/en-us/kb/5020779)
- **Loss and jitter:**
  - [MMORPG tolerance study](https://scholars.lib.ntu.edu.tw/entities/publication/f0ce837f-e399-43f7-8994-ed8fed896ad0)
  - [klauspost/reedsolomon](https://pkg.go.dev/github.com/klauspost/reedsolomon)
  - [Gilbert-Elliott](https://core.ac.uk/works/16939205)
  - [MTR interpretation](https://itldc.com/de/kb/understanding-mtr-and-traceroute-why-packet-loss-on-transit-hops-usually-is-not-a-problem/)
  - [Delivery Optimization](https://learn.microsoft.com/en-us/windows/deployment/do/waas-delivery-optimization-reference)
  - [QOSSetFlow](https://learn.microsoft.com/en-au/windows/win32/api/qos2/nf-qos2-qossetflow)
  - [WLAN opcodes](https://learn.microsoft.com/en-us/windows/win32/api/wlanapi/ne-wlanapi-wlan_intf_opcode)
- **Relays:** [Oman-IX](https://www.peeringdb.com/ix/4371), [Awasr IGW](https://www.peeringdb.com/net/36226), [UAE-IX](https://www.de-cix.net/en/about-de-cix/news/uae-ix-extends-its-presence-in-the-united-arab-emirates), [DataPacket Fujairah](https://www.datapacket.com/datacenters/fujairah), [OCI free tier](https://docs.oracle.com/en-us/iaas/Content/FreeTier/resourceref.htm), [Vultr BGP](https://docs.vultr.com/configuring-bgp-on-vultr)
- **Games and regions (added Oct 2026):** [EA Battlefield 6 Secure Boot](https://www.ea.com/games/battlefield/battlefield-6/news/secure-boot-information), [CS2 telemetry HUD (Steam)](https://help.steampowered.com/faqs/view/5E6F-5B36-5485-F6B9), [ExitLag non-game apps KB 273](https://www.exitlag.com/knowledgebase/273), [ExitLag v5.18 shaper](https://www.exitlag.com/blog/exitlag-version-5-18/), [GearUP server choice](https://www.gearupbooster.com/support/how-to-use-gearup-booster.html), [ASUS + GearUP console](https://press.asus.com/news/press-releases/asus-gearup-console-booster-apac)
- **Legal (secondary, needs counsel):** [UAE VPN misuse fines](https://www.khaleejtimes.com/business/tech/vpn-uae-6-million-downloads-dh2-million-fine-misuse?amp=1), [Oman TRA VPN warning](https://meatechwatch.com/?p=55356), [Oman Observer on VPN legality](https://www.omanobserver.om/ampArticle/1194989)
- **Shell:** [code signing options](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options), [SmartScreen reputation](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation), [unpackaged toasts](https://learn.microsoft.com/en-us/windows/apps/design/shell/tiles-and-notifications/send-local-toast-desktop-cpp-wrl), [MSIX can't install drivers](https://techcommunity.microsoft.com/t5/msix/msix-compatibility/m-p/2430380)
