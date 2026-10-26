# Architecture

Goal: turn the simulator into an app that optimizes **real game traffic on
Windows**. Pick a game, press Boost, launch the game: its UDP packets are
captured transparently (no game settings to change), sent over real relay
servers with multipath duplication, and the replies are put back into the
game's socket. The UI shows real before/after numbers.

This document describes how the pieces fit together and the contracts between
them (packages, wire protocol, HTTP API). Keep it in sync with the code: a
change to a contract updates this file in the same pull request. What comes
next is in the [roadmap](roadmap.md).

## What "works" means for this MVP

1. `Lowpath.exe` is one self-contained Windows binary. Double-click → UAC
   prompt → desktop window (WebView2) with the existing UI.
2. **Route mode** (default): WinDivert captures outbound IPv4 UDP sent by the
   selected game's processes and tunnels it. Everything else on the PC is
   untouched. TCP and IPv6 stay direct (documented limitation).
3. Relays are real servers the user adds in Settings → Relays (address + key).
   A built-in **loopback test relay** (an egress inside Lowpath.exe on
   127.0.0.1) lets anyone verify capture end to end without a server.
4. `relay` runs on a Linux VPS with an access key; `deploy/deploy-relay.ps1`
   installs it over SSH as a systemd service.
5. Before = ICMP ping from this PC to the game server. After = multipath
   probe RTT to the egress relay + the egress relay's ICMP ping to the game
   server. If the server ignores ping, the UI says so instead of inventing a
   number.
6. `lpcheck.exe` (a tiny STUN client) proves the route: run it as a custom
   game and it prints the public IP the internet sees — the relay's IP when
   Lowpath is routing it.
7. Demo mode (`-demo`) keeps the simulated network for showing the product
   without servers.
8. Lowpath is free and open source (MIT). There are no accounts, sign-up,
   trials, plans, paywall, billing or device limits: the app opens straight
   into the game library and Apply/Boost is always available. The only gate
   on the API is the per-launch local session below, which keeps other
   programs on the PC out; it is not a login.

## Overlay protocol v2 (done — `internal/proto`)

- Header gains `Token uint64` right after `type`. `proto.TokenFromKey(key)`
  derives it; `0` = open relay. Relays started with a key drop every packet
  whose token differs. Forwarding hops pass packets through untouched.
- A `TProbe` may carry `Addr` = the game server. The egress replies with
  `TProbeReply` (same header, type changed) whose payload is
  `proto.AppendServerRTT(nil, rtt)`; `-1` → unknown. Clients decode with
  `proto.ParseServerRTT(payload)`.
- Sequence numbers are per flow and direction. The egress starts each
  flow's downstream numbering at a random value, and `dedup.Window` treats a
  number `dedup.RestartGap` (4 × window) or more behind its newest one as a
  new sender (relay restart, egress failover) and starts over instead of
  rejecting it as too old.

## Capture — `internal/divert`, `internal/capture`

`internal/divert`: pure-Go (no cgo) bindings to WinDivert 2.2.2 via
`syscall.NewLazyDLL` / `windows.NewLazyDLL` with an absolute DLL path.

```go
//go:embed bin/WinDivert.dll bin/WinDivert64.sys   (already in internal/divert/bin)
func Install(dir string) error            // write both files into dir if missing/different (compare bytes), 0644
func Load(dir string) error               // load dir\WinDivert.dll (once)
type Handle struct{ ... }
func Open(filter string, layer Layer, priority int16, flags uint64) (*Handle, error)
func (h *Handle) Recv(buf []byte, addr *Address) (int, error)
func (h *Handle) Send(pkt []byte, addr *Address) (int, error)
func (h *Handle) Shutdown() error; func (h *Handle) Close() error
func (h *Handle) SetParam(p Param, v uint64) error
func CalcChecksums(pkt []byte, addr *Address) error
func CompileFilter(filter string, layer Layer) error  // WinDivertHelperCompileFilter; works WITHOUT admin
type Address struct{ ... }  // exact 80-byte WINDIVERT_ADDRESS layout, with accessors Outbound()/SetOutbound(), Loopback(), IPv6(), IfIdx(), SubIfIdx(), SetNetwork(ifIdx, subIfIdx)
var ErrNeedAdmin  // returned by Open on ERROR_ACCESS_DENIED
```
Non-Windows: stub file returning `ErrUnsupported`.

`internal/capture`:

```go
type Config struct {
    DriverDir string               // where divert.Install put the files
    Processes []string             // image names to capture, case-insensitive ("cs2.exe")
    PIDs      []uint32             // extra exact PIDs to capture (optional)
    Exclude   []netip.AddrPort     // relay endpoints: never captured
    OnPacket  func(local, remote netip.AddrPort, payload []byte) bool
        // called for every captured game packet; payload is only valid during
        // the call. true = consumed (tunneled or dropped by kill switch);
        // false = reinject the original packet unchanged (goes out directly).
    Log *slog.Logger
}
func Available() error   // nil, or ErrNeedAdmin / ErrNoDriver / ErrUnsupported (non-Windows)
func Start(cfg Config) (*Capture, error)
func (c *Capture) Inject(local, remote netip.AddrPort, payload []byte) error
    // deliver a reply to the game: builds IPv4+UDP src=remote dst=local,
    // inbound, on the interface the flow's outbound packets used. Goroutine-safe.
func (c *Capture) Stats() Stats  // Captured, Passed, Injected, Errors uint64; GamePIDs []uint32; LastError string (json snake_case tags)
func (c *Capture) Close() error  // must always restore normal networking
```

How it works:
- One WinDivert handle at `LAYER_NETWORK` with a filter like
  `outbound and !loopback and ip and udp and udp.DstPort != 53 and
  ip.DstAddr < 224.0.0.0 and <not private LAN ranges> and <not relay endpoints>`.
  Validate the exact string with `CompileFilter` in a unit test. WinDivert 2.2
  can't negate a parenthesized group, so "not in a range / not a relay" is
  written as `(ip.DstAddr < lo or ip.DstAddr > hi)` and
  `(ip.DstAddr != X or udp.DstPort != P)`; at most 64 relay endpoints go in.
- Per packet: parse IPv4 + UDP (skip fragments, options OK). Owner lookup by
  local UDP port → PID via `GetExtendedUdpTable(UDP_TABLE_OWNER_PID)`, PID →
  image name via a Toolhelp32 snapshot. Cache port→verdict; refresh the game's
  ports in the background every second so game packets hit the cache; on a
  miss, query synchronously (the very first packet of a game must not leak).
  Never capture our own PID.
- Game packet → `OnPacket`; if it returns false, reinject unchanged.
  Non-game packet → reinject unchanged immediately.
- Remember per flow (local, remote) the `IfIdx/SubIfIdx` for `Inject`.
- `Inject` builds the packet in Go, then `CalcChecksums`, then `Send` with an
  inbound network address.
- Several receive goroutines (2–4) on the handle for throughput.
- Packet parse/build lives in a platform-neutral file with unit tests.

## Engine — capture mode

Keep proxy mode and every existing test passing. Additions:

```go
type PathSpec struct {
    ID, Label, Region, Continent string
    Addr   netip.AddrPort
    Key    string `json:"-"`                // relay access key → token on every packet of this path
    Egress string `json:"egress,omitempty"` // paths sharing an egress relay share this id ("" = one shared group)
}

type Config struct {
    // proxy mode (existing): Listen != ""; Target; Direct (UDP-echo baseline, demo)
    // capture mode: Listen == "" and Deliver != nil
    Deliver func(local, remote netip.AddrPort, payload []byte) // first-copy replies to the game
    ...existing fields...
}

func (e *Engine) Send(local, remote netip.AddrPort, payload []byte) bool
    // capture mode entry point; concurrency-safe; must not retain payload.
    // false only when in fallback with KillSwitch off (caller sends it direct).
```

- Flows keyed by (local, remote). Proxy mode uses remote = Target.
- Every packet a path sends carries `Token: proto.TokenFromKey(spec.Key)`
  (compute once per path).
- **Egress groups:** duplicates may only go to paths with the same `Egress`
  (two egress relays would deliver the same packet to the server twice from
  different IPs). Pick the best path; the active set is the top-k of its
  group. Switch to another group only when the current group has no live
  path (failover), and log an event saying the server will see a new IP.
- **Primary server**: the remote with the most upstream packets in the last
  ~5 s. Snapshot lists up to 8 servers.
- **Before** in capture mode: `icmp.Ping` to the primary server's IP, 5 Hz,
  each ping in its own goroutine, recorded in the existing tracker.
  `BeforeKnown` = at least one reply in the window.
- **After**: the after-probe (and per-path probes) carry `Addr` = primary
  server. Replies' server RTT (if present) is added to the sample. Expose
  `ServerLeg` (ms, latest) and `ServerLegKnown`.
- Proxy/demo mode keeps the UDP-echo direct baseline; probes carry Target so
  the demo egress reports its (≈0) leg. `Config.PingBefore` (set by the app
  for proxy mode against a real server) measures "before" with `icmp.Ping` to
  Target instead, and sends the server no echo probes.
- `Analyze(ctx, specs, AnalyzeOptions{Direct netip.AddrPort /*UDP echo, demo*/, Server netip.Addr /*ICMP, real*/}, dur)`
  → `Analysis{Direct Stats, DirectKnown bool, Paths map[string]Stats}`; path
  RTTs include the relay's server leg when known. With no server known,
  `DirectKnown=false` and paths show relay RTT only.
- Snapshot JSON additions: `mode` ("proxy"|"capture"), `servers`
  (`[{addr, pkts_up, pkts_down, last_seen, primary}]`), `server_leg_ms`,
  `server_leg_known`, `before_known`. `listen` is "" in capture mode.
- Stats JSON gains `server_ms` (relay→server leg included in `rtt_ms`, 0 if unknown).

## Relay, lpcheck, deploy

- `relay.ListenEgress(addr, log)` keeps its signature (open relay, private
  destinations allowed — used by demo, tests and the loopback relay).
  New `relay.ListenEgressOpts(addr string, o Options, log)` with
  `Options{Key string; AllowPrivate bool; Ping func(netip.Addr, time.Duration) (time.Duration, error); PingInterval time.Duration}`.
- Token check when a key is set (silently drop mismatches; count them).
- Block destinations that are loopback, private, link-local, multicast,
  unspecified, CGNAT, reserved, IPv6 forms that embed an IPv4 address
  (NAT64, 6to4, Teredo, IPv4-compatible/translated) or the relay's own
  addresses unless AllowPrivate.
- Limits (a relay is a public service): replies of a flow go to at most 8
  client addresses (the ones used within 3 s of the newest; while the client
  is quiet they keep getting replies until the flow expires); a flow ends
  60 s after its last upstream packet, or 10 s if the server never answered;
  with all 4096 flow slots taken, flows quiet both ways for 10 s are
  reclaimed (at most once a second); server packets over 16 KiB are dropped
  and counted (`too_big`).
- Server-leg pinger: every destination seen in data or probes is pinged
  (first ping immediately, then every PingInterval ≈ 1 s) while it is in use
  (forget after 30 s idle); smoothed RTT (EWMA); unknown after 3 misses.
- Probe replies carry `proto.AppendServerRTT`.
- `cmd/relay` flags: `-listen :40000`, `-key` (or env `LOWPATH_RELAY_KEY`),
  `-forward host:port` (forwarder hop), `-allow-private`, `-stats 60s`
  (periodic log of flows/packets/drops).
- Forwarding hop: `relay.ListenForwarder(addr string, next netip.AddrPort, log)`
  → `*Forwarder` (`Addr`, `Serve(ctx)`, `Close`, `Stats() ForwarderStats`).
  One socket toward the next hop per client address; only packets that parse
  as overlay headers pass; a client address loses its socket after 60 s quiet
  (10 s if the next hop never answered it); at most 4096, and when full an
  unanswered one is evicted for a newcomer (answered ones never are).
- `cmd/lpcheck`: STUN binding requests (RFC 5389) at 20 Hz to
  `stun.l.google.com:19302` (flag `-server`), runs `-for 10s` (0 = forever);
  prints RTT avg/jitter/loss and the XOR-MAPPED-ADDRESS (public IP:port the
  server saw). Pure stdlib; the message codec is `internal/stun`.
- `deploy/relay/install.sh` (systemd unit `lowpath-relay`, runs as root for
  ICMP, opens UDP port with ufw/firewalld if present, idempotent) and
  `deploy/deploy-relay.ps1 -Server user@host [-Port 40000] [-Key ...]` (uses
  Windows' built-in `ssh`/`scp`; uploads `dist/relay-linux-amd64` or
  `-arm64`; prints the address + key to paste into Lowpath).
- Tests: wrong token dropped, private destination blocked, probe reply carries
  the (fake) pinger's RTT, open relay still works, the limits above.

## Location and ISP — `internal/stun`, `internal/diag`

- `internal/stun`: the RFC 5389 Binding codec (shared with lpcheck) and
  `Client.Bind(ctx, "udp4"|"udp6", server)`: one transaction, resent with
  the same id after 250 ms, the wait doubling up to 1 s, 4 sends (about
  2.75 s). RFC 5389 suggests 500 ms and 7 sends (39.5 s), too slow for a
  status line. RTT is reported only when the first send was answered (a
  resend is byte-identical, RFC 7982). A socket error (ICMP unreachable)
  ends the round early instead of spinning.
- `diag.Locator` answers "where am I on the internet". Per family (IPv4 and
  IPv6, in parallel) the first `AddrFinder` that answers wins:
  `STUNFinder` (stun.l.google.com:19302 and stun.cloudflare.com:3478 at
  once, first answer wins, 5 s cap including DNS), then RIPEstat
  `whats-my-ip` over HTTPS pinned to that family (for networks that block
  UDP). A public address goes to the `NetworkLookup`: RIPEstat
  `network-info` (prefix, origin ASNs), then `as-overview` (holder, split
  into handle and ISP name) and `maxmind-geo-lite` (country, city) in
  parallel; those two may fail alone (`partial`). `CachedLookup` keeps an
  answer per address for 24 h, a partial one 10 min, a failure 30 s.
  Private, CGNAT and loopback answers are never sent to the registry.
- One lookup runs at a time, with a 12 s budget that does not depend on a
  caller who gives up; a result is reused for 2 min unless `refresh=1`.
  States: `ok`; `partial` (IP known, ISP not: `message` says why);
  `offline` (no IP: `message` says why). No IPv6 is normal and only sets
  `ipv6_note`.
- Lowpath never captures its own process, so this is the user's own line
  even while a game is routed through a relay.
- `/api/diagnostics` includes the last location with addresses cut to /24
  (IPv4) and /48 (IPv6); it never starts a lookup.

## App, desktop shell, build

- `cmd/lowpath` replaces `cmd/lowpathd`. Flags: `-addr 127.0.0.1:7878`,
  `-data`, `-demo`, `-relays file.json` (optional import), `-headless` (no
  window), `-open` (browser instead of window), `-no-elevate`.
- Elevation: bind the port first (if another copy serves it, focus that one
  and exit, no UAC prompt); then, if not admin and not `-no-elevate`,
  relaunch with `runas` (ShellExecuteW) passing `-wait-pid <self>`; on UAC
  cancel keep running unelevated (route mode shows "needs admin").
  `POST /api/elevate` does the same on demand; the child waits for the parent
  to exit before binding.
- Window: `github.com/jchv/go-webview2` (pure Go), title "Lowpath",
  1100×720, min 920×600, icon from resources. Closing the window shuts down
  cleanly (stop session → capture closed → networking restored). Logs go to
  `<data>\lowpath.log` and stderr.
- Resources: app icon + manifest (asInvoker, DPI aware) via `go-winres` →
  `cmd/lowpath/rsrc_windows_amd64.syso`.
- WinDivert files extracted to `<Program Files>\Lowpath\driver`, the folder
  resolved with `KnownFolderPath(FOLDERID_ProgramFiles)` (not the
  environment). Fallback `<exe dir>\driver` only when not elevated: an
  elevated process never loads the DLL from a user-writable folder.
- Store additions (state.json): relays, custom games, per-game last servers,
  settings `capture_mode` ("route"|"proxy", default route on Windows) and
  `loopback_relay` (bool, default false).
- Session in route mode: start engine (capture mode) + capture with the
  game's processes; Exclude = all relay endpoints; wire `capture.OnPacket →
  engine.Send`, `engine.Deliver → capture.Inject`. Record primary servers per
  game every few seconds. Stop tears down capture first, then engine.
- `build.ps1`: npm ci/build → go-winres → `dist\Lowpath.exe` (`-H windowsgui`),
  `dist\lpcheck.exe`, `dist\relay-linux-amd64`, `dist\relay-linux-arm64`,
  copy `deploy\`. Plus `go vet` + `go test`.

### HTTP API

Bootstrap adds:
```json
"platform": {"os": "windows", "admin": true,
             "capture": {"available": true, "reason": ""}},
"relays": [RelayView]
```
`RelayView = {id, label, region, continent, addr, egress, has_key, builtin}`
(the loopback relay appears with `builtin: true` when the setting is on).

Settings adds `"capture_mode": "route" | "proxy"` and `"loopback_relay": bool`.

Game view adds `"custom": bool` and keeps `"processes": string[]`.

| Method | Path | Body | Response |
|---|---|---|---|
| GET | /api/relays | | `{relays: RelayView[]}` |
| POST | /api/relays | `{label, region, continent, addr, key, egress}` | `RelayView` (addr may be host:port; resolved on save) |
| DELETE | /api/relays/{id} | | `{ok: true}` |
| POST | /api/relays/test | `{id}` or `{addr, key}` | `{ok, rtt_ms, loss_pct, message}` |
| GET | /api/processes | | `{processes: [{pid, name, udp_ports}]}` (only processes with UDP sockets) |
| POST | /api/games/custom | `{name, process}` | `Game` (id `custom-<slug>`) |
| DELETE | /api/games/custom/{id} | | `{ok: true}` |
| POST | /api/elevate | | `{ok: true}` then the app restarts elevated |
| GET | /api/location[?refresh=1] | | `{state, message, at, ipv4, ipv6, ipv6_note}`; each address `{ip, source, asn, asns, holder, as_name, isp, prefix, country, city, partial, lookup_error}` or null. 504 `location_timeout` |

Live adds `"capture": null | {"mode": "route", "captured", "passed",
"injected", "errors", "game_pids": number[], "last_error": string}`.
Session adds `"capture_mode"`.

New error kinds: `needs_admin` (409), `no_relays` (409), `capture_failed` (500),
`unauthorized` (401).

**Session.** Every `/api` request except the two below needs the
`lowpath_session` cookie (HttpOnly, SameSite=Strict, no expiry), a random
secret made at each launch; otherwise 401 `unauthorized`. The app opens its
window (or browser, with `-open`) at `app.Server.LaunchPath()`; `-headless`
prints that URL to stderr only, never to the log.

| Method | Path | Response |
|---|---|---|
| GET | /api/session?t=<one-time token> | 303 to `/` with the cookie; 403 `bad_launch_token` once used |
| GET | /api/version | `{version}`: how a second launch finds a running copy |

## UI

- Settings → **Relays** tab: list (label, addr, region, key yes/no, latency
  from Test), Add form (label, address, key, region, continent, "forwards to
  egress" select), Delete, Test, "Use loopback test relay" toggle with an
  explanation, and a short "Deploy your own relay" help with the
  `deploy-relay.ps1` command.
- Settings → Network: **Your connection** ("<city>, <country> · <ISP>
  (AS<n>)", public IPv4 and IPv6, prefix, Refresh; "—" and the reason when
  unknown), looked up when the pane opens. Then capture mode (Route —
  recommended / Proxy).
- No relays → game panel shows an onboarding card (add a relay / enable the
  loopback test relay) instead of a broken Apply.
- Route mode without admin → banner with **Restart as administrator**
  (`POST /api/elevate`).
- Optimizing in route mode: "Waiting for game traffic — launch the game now"
  until packets flow; then the **Game servers** list (addr, primary, up/down
  packets) and capture counters. Proxy mode keeps the existing
  "point your game at 127.0.0.1:port" hint.
- Before/after strip: `before_known=false` → "Server ignores ping"; after
  without server leg → "to relay" suffix.
- Library: **Add game** → modal listing running processes with UDP sockets
  (from `/api/processes`), name field, add; custom games get a delete action.
- Demo-only controls (Network lab) only when `demo` is true.

## Verification

- `go vet ./...`, `go test ./...`, `GOOS=linux go build ./cmd/relay`,
  `npm run build`.
- Non-admin, real internet: engine (capture mode, fake capture) + loopback
  egress tunnels a STUN request to Google and gets the reply
  (`$env:LOWPATH_INTERNET=1; go test -run TestInternet ./internal/engine ./internal/relay`).
- `divert.CompileFilter` accepts the generated filter; `divert.Open` without
  admin returns `ErrNeedAdmin`.
- Admin (needs one UAC click): Lowpath.exe → loopback relay → custom game
  `lpcheck.exe` → Apply → `lpcheck.exe` shows replies with capture counters
  rising in the UI.

## Known limitations

UDP over IPv4 only (TCP/IPv6 go direct). Some anti-cheats (Vanguard, FACEIT)
may refuse to run alongside the WinDivert driver. Unsigned exe → SmartScreen
warning. Real benefit needs relays on well-peered servers near the game's
datacenter; the loopback relay only proves the pipeline.
