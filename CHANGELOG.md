# Changelog

All notable changes to Lowpath are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every pull request that changes behaviour adds a line under **Unreleased**.

## [Unreleased]

### Added
- Route trace (MTR) in Settings → Diagnostics: loss and delay at every
  router on the way to a relay, game server or any host, 200 probes per
  hop over ICMP (no admin rights on Windows), or with the relay's own UDP
  port as the destination. Loss only counts when it persists to the
  destination, so routers that limit their replies aren't blamed; the
  trace says where real loss starts and exports a WinMTR-style text
  report (#13).
- Fail-open watchdog: a stuck or crashed packet handler, a stopped driver
  or a killed app puts the game back on the direct connection within a
  second; the app ends the session and says why, and the next start cleans
  up after a run that never closed (#7).
- Linux and macOS: "Add game" lists programs with UDP sockets (`/proc` on
  Linux, libproc on macOS), and the library finds installed and running
  games in Steam libraries and Wine, Proton, Lutris and Heroic installs (#43).
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

### Fixed
- `scripts/ci.ps1 -Report` no longer refuses to report after a UI change:
  the generated `ui/dist` output doesn't count as an uncommitted change.

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
