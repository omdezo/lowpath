# Changelog

All notable changes to Lowpath are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every pull request that changes behaviour adds a line under **Unreleased**.

## [Unreleased]

### Added
- Network simulator: Gilbert-Elliott burst loss (`p`, `r`, `1-k`, `1-h`,
  stepped per packet or in time) and shared segments, so lanes behind one
  bursty hop such as home Wi-Fi lose packets together; seeded random
  choices make lab runs repeatable (#55).
- Passive line-load detector: without a bufferbloat test, the line rate is
  now the most traffic seen in the last 10 minutes instead of since start,
  so a fast LAN copy no longer hides later downloads until a restart, and a
  boost suggests running the test once. On Linux, traffic to LAN peers is
  left out of "other traffic" and the banner names the program filling the
  line (per-connection counters from sock_diag, no root needed). Analyze
  and the startup route mapping wait for a running bufferbloat test, and a
  measurement the test still loaded says so and doesn't seed the boost
  (#116).
- Router pinger and segmented Home / ISP / Lowpath / Game server bar in
  Settings → Diagnostics: the default gateway is pinged 20 times a second
  while the tab is open and during a boost, with loss, median, p99, spikes
  and a 60-second chart, and the verdict uses it; a router that stops
  answering counts as loss, not as one that ignores ping (#10).
- Wi-Fi link in Settings → Diagnostics: signal strength, band, channel,
  width, Wi-Fi generation and link rate from the Native Wifi API (no
  location access needed), and with *Check channel* the router's own
  channel load (BSS Load) and the other networks on its channel. While a
  game is boosted over Wi-Fi, background scans are paused and streaming
  mode is on (Settings → Optimization, on by default), applied again after
  a reconnect and reverted when the boost ends or Lowpath closes. The
  verdict counts a weak signal, 2.4 GHz or a busy channel against the Wi-Fi,
  the route bar's Home part says *Watch* when the router pings are clean
  but the signal is weak, and the report gains a Wi-Fi line. Linux shows
  the signal strength (#11).
- Dev-mode background service `LowpathSvc` (Windows): `lowpathsvc install`
  asks for administrator rights once, and from then on the Lowpath window
  runs unelevated and drives the service over the named pipe
  `\\.\pipe\lowpath`, so Boost works after a reboot with no UAC prompt (#6).
  The window starts a stopped service by itself, says "Lost connection to
  the Lowpath service" while it restarts after a crash, and refuses a pipe
  served by any other process; Settings → Network shows whether the service
  is in use, or why this window doesn't use it. `lowpathsvc status` and
  `uninstall` too; `-no-service` keeps the engine in the window. The
  service's data folder is created admins-only in one step and secured
  again at every start; a folder users could change is moved aside. On
  macOS the socket is `/var/run/lowpath/api.sock`.
- Desktop shell on Linux and macOS: the UI opens in an app window
  (WebKitGTK or a Chromium app window on Linux, WKWebView on macOS, the
  default browser otherwise), and closing it quits and stops the boost. A
  second start brings the running copy forward instead of starting another.
  On Linux route mode asks for your password once (polkit) to give the
  program file, made private to you, its two network capabilities, and
  keeps running as you; the first start adds Lowpath to the app menu. A
  background service started by hand (`lowpathsvc run` as root) is used
  first, and then nothing is asked. The build makes a universal
  `Lowpath.app` for macOS. Logs go to `~/.local/state/Lowpath` and
  `~/Library/Logs/Lowpath` (`-logs` to choose), and SIGHUP stops the app
  cleanly (#44).
- Verdict in Settings → Diagnostics: one plain answer to "why am I
  lagging?" (Wi-Fi or home network, a full line, the ISP's route, the game
  server, or already optimal), with a confidence level, the measurements
  behind it and an exportable text report (#14). It says when the PC is
  offline, ignores a route trace taken on another connection, and counts a
  carrier-grade NAT hop as the ISP's, not the home network.
- Conflict detector: ExitLag, NoPing and other game boosters, the Windows
  Packet Filter driver they leave behind, WinDivert loaded by another
  program, tools such as clumsy, and a VPN that carries all traffic are
  named in a banner with what to do and a "Why?". Route mode won't start
  next to a running booster (Boost says to close it first, or Boost
  anyway); Settings → Network lists everything found. On Linux and macOS a
  VPN that takes over with `0/1`+`128/1` routes or policy routing is found
  too (#9).
- Route mode on Linux: a TUN device, an nftables mark and a policy-routing
  rule send only the selected game's UDP through Lowpath, and the relays'
  replies go back through the TUN; every other program keeps its normal
  route, and everything is removed on Stop, on a crash or when the app is
  killed (#41). Without root it marks only its own user's sockets, and
  datagrams bigger than the link's MTU are fragmented when sent on, as the
  kernel would have done.
- Route trace (MTR) in Settings → Diagnostics: loss and delay at every
  router on the way to a relay, game server or any host, 200 probes per
  hop over ICMP (no admin rights on Windows), or with the relay's own UDP
  port as the destination. Loss only counts when it persists to the
  destination, so routers that limit their replies aren't blamed; the
  trace says where real loss starts and exports a WinMTR-style text
  report (#13).
- Route trace without root on Linux and macOS: when the raw ICMP socket
  isn't allowed, Linux traces through an unprivileged ping socket
  (`net.ipv4.ping_group_range`, open on systemd distributions) and reads
  the routers' answers from its error queue, and macOS through its
  datagram ICMP socket; IPv4 and IPv6. The trace only asks for root when
  neither works (#137).
- Fail-open watchdog: a stuck or crashed packet handler, a stopped driver
  or a killed app puts the game back on the direct connection within a
  second; the app ends the session and says why, and the next start cleans
  up after a run that never closed (#7).
- Linux and macOS: "Add game" lists programs with UDP sockets (`/proc` on
  Linux, libproc on macOS), and the library finds installed and running
  games in Steam libraries and Wine, Proton, Lutris and Heroic installs (#43).
- "Current server" card on every game page: the server the game is
  really talking to, with its network (AS, ISP), city checked against the
  measured ping, and live ping, jitter and loss, also for games with no
  catalog entry (#20). Only UDP servers are saved for proxy mode, a flow
  watch stopped by the OS is retried, and the diagnostics report leaves
  out map coordinates.
- Valve server list: Counter-Strike 2, Dota 2 and Deadlock show their Steam
  Datagram Relay data centers with live ping and loss on the game page,
  refreshed in the background and cached for offline starts (#16).
- "Your connection" in Settings → Network: public IPv4/IPv6 address (via
  STUN, as game servers see it), network prefix, ISP, AS and city, with a
  clear reason whenever something can't be found (#15).
- Bufferbloat test (Settings → Diagnostics): 20 s idle/download/upload
  latency test graded A+ to F, plus a passive detector that warns during a
  boost when other traffic fills the line (#12).
- Platform layer: capture, socket-owner lookup, privileges, game detection,
  icons and folders sit behind per-OS interfaces; the app builds for Windows,
  Linux and macOS, and explains when a feature isn't available on the
  current OS (#40).
- Local CI publishes its result as the `local-ci` status check on pull
  requests (`scripts/ci.ps1 -Report`).
- CODEOWNERS, this changelog, and the pull request process in
  `CONTRIBUTING.md`.
- An `addrs` stage in `scripts/ci.ps1` and `scripts/ci.sh` fails when a
  test, e2e or doc file names a real public IP address; only the
  documentation ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24,
  2001:db8::/32) and a short allow-list of public resolvers and other
  necessary addresses pass (#131).

### Changed
- Test fixtures and docs use documentation addresses and AS numbers
  (AS64496–AS64511) and neutral example networks and locations (#131).

### Fixed
- A boost no longer falls back to the direct route for a second right after
  it starts when the PC is busy: a route in use is judged on what its
  probes said, so one whose first probes are still in flight (or not sent
  yet) at the first selection tick keeps carrying the game. Routes that
  are all dead still fall back, about one second later than before, once
  their first probes go unanswered for 1 s, and within about 3 s of
  the start even if no probe could be sent (#178).
- The service's Unix socket now reads a refused account's request before
  answering 403, so that window always learns it isn't served ("only
  serves accounts signed in at this computer") and runs the engine itself,
  instead of sometimes getting an "unsolicited response" transport error
  because the answer arrived before its request was sent (#197).
- Route mode on Linux notices when another program removes its routing
  rule (systemd-networkd does this to rules its `.network` files don't
  declare) or, on kernels before 5.12, its nftables table (a firewall's
  `nft flush ruleset`): it puts them back within about a second, counts
  and logs each time (*Route fixes* in the live view), and ends the boost
  with the reason when it can't put them back or another program keeps
  removing them, instead of saying the game is boosted while its packets
  take the normal connection (#151).
- A "Lowpath Flows" ETW session left behind by a Lowpath that crashed while
  a game ran is stopped when Lowpath starts again, instead of keeping
  Microsoft-Windows-Kernel-Network enabled and its buffers filling until
  the next game. It happens in the background and never delays start-up;
  without administrator rights it is skipped (#130).
- Current server card: a lookup spoiled by a slow or busy RIPEstat (no
  network, or no ISP name or city) is asked again after 3 s instead of
  being kept for 1 min, or for 1 h when only a part was missing; a registry
  that stays slow is asked less and less often, up to once a minute, and
  the card keeps showing what it already knew meanwhile (#145).
- The background service's Unix socket on Linux and macOS no longer serves
  every local account: root, the service's own account and accounts signed
  in at the computer (a logind session, a utmpx session on macOS, or a
  person's uid where the system keeps no session list) are served, and
  daemon accounts such as `nobody` get `403 Forbidden`. The window refuses
  a socket that root doesn't serve, and says why when it isn't served
  (#163).
- Settings → Network: a slow first RIPEstat reply no longer leaves the ISP
  unknown. A registry request with no answer after 2.5 s (or a 502) gets a
  second copy on a fresh connection and the first answer wins; a lookup
  spoiled by a timeout is kept for 3 s instead of 30 s, and the pane asks
  once more by itself a few seconds later (#112).
- `scripts/ci.ps1 -Report` no longer refuses to report after a UI change:
  the UI build goes to the ignored `ui/dist/app`, so it is never an
  uncommitted change.
- Building the UI leaves `git status` clean: `npm run build` writes to the
  ignored `ui/dist/app`, and a tracked placeholder page lets a fresh clone
  pass `go vet` and `go test` before the UI is built. A program built
  without the UI shows that page, with how to build it, and logs a warning.
  The local CI's `ui` stage fails if the build changes what `git status`
  shows, and the `-Report` clean-tree check no longer makes an exception
  for `ui/dist`, so an edited placeholder counts (#121).
  the generated `ui/dist` output doesn't count as an uncommitted change.
- The relay and engine tests no longer fail on a busy machine: they wait
  for the forwarding hop's counters and the engine's live samples instead
  of sleeping, check the order of measured routes rather than RTT
  ceilings, and the relay-restart test keeps its burst within what
  loopback receive buffers hold (#114).
- Route mode (Windows): closing the capture handle waits for the WinDivert
  calls still running on it, so a late inject from the engine or a worker
  that comes back after a fail-open can no longer reach the driver through
  a released handle that Windows may have reused (#128).
- The app tests no longer fail when the test binary itself gets a small
  PID, as in a fresh container or WSL's `docker-desktop`: their fake
  processes take PIDs that can't be the binary's own (#189). The Wi-Fi
  scan pause tests no longer fail on a machine too busy to run the test
  for a couple of seconds: they wait for the refresh and for the revert
  to finish instead of giving them 2 s and 300 ms (#190).
- The end-to-end test waits for the state each check reads instead of a
  fixed time: the bufferbloat test and each route trace wait for their
  own click's start and a newer result (no more reading the previous UDP
  trace as the ICMP one), the verdict waits until the page shows the
  title the API gives, and "Session ends when the game exits" waits for
  the session to end in the API, then in the window. The log now says
  when the game was first seen during a session, so the check can say
  which half missed (#177, #201).

## [0.2.0] - 2026-10-26

First public release.

### Added
- Per-process UDP capture on Windows (WinDivert) with injection of replies.
- Multipath relay overlay with packet duplication and first-copy-wins dedup.
- Linux relay with access keys, destination policy, flow limits and
  server-leg ping; forwarding hops; one-command deploy over SSH.
- Honest before/after measurements (ICMP to the real game server, relay path
  plus the relay's own ping).
- One-button UI with real game artwork, route map, live graph and details.
- Built-in loopback test relay and the `lpcheck` STUN route checker.
- Unit, internet and end-to-end tests; local CI pipeline.

### Removed
- Accounts, sign-up, trials and plans: Lowpath is free and open source.

[Unreleased]: https://github.com/omdezo/lowpath/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/omdezo/lowpath/releases/tag/v0.2.0
