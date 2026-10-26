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

## Platform layer — `internal/platform`, `internal/owner`

Everything OS-specific sits behind small interfaces in `internal/platform`,
so the engine, the app and the UI are the same on every OS. `platform.Native()`
returns the implementation chosen at build time (`native_windows.go`,
`native_other.go`); `app.Config.Platform` takes it (nil = `Native()`), and
tests pass `internal/platform/fakeplatform` instead, which runs the whole app,
route mode included, without admin rights, a driver or a particular OS.

```go
type Platform struct {
    OS         string          // runtime.GOOS of the implementation
    Capture    CaptureBackend  // = capture.Backend
    Owners     OwnerResolver   // = owner.Resolver
    Privileges Privileges
    Games      GameDetector
    Icons      IconProvider
    Paths      Paths
    Flows      FlowWatcher
    Inventory  SystemInventory // = compat.System
}
type Privileges interface {
    Elevated() bool                // administrator (Windows) or root
    CanElevate() bool              // Relaunch can show a prompt
    Relaunch(args []string) error  // ErrElevateCancelled when declined
}
type GameDetector interface {
    Running() map[string]bool          // lower-case image names
    RunningExe(names []string) string  // "" = none, or the OS won't tell
    SteamLibraries() []string
    SteamRoots() []string
    WinePrefixes() []string            // drive_c of Wine/Proton prefixes (none on Windows)
    LauncherInstalls() []string        // game folders launchers report (Heroic/Legendary)
}
type IconProvider interface{ ExeIcon(path string, size int) (image.Image, error) }
type FlowWatcher interface {
    // every process's UDP/TCP sends and receives (addresses, ports, size;
    // never payload) until Close; detect.ErrNeedAdmin / errors.ErrUnsupported
    Watch(onEvent func(detect.FlowEvent)) (detect.FlowWatch, error)
}
type Paths interface {
    DataDir() (string, error)   // settings, relays, log
    CacheDir() (string, error)  // rebuildable (window profile)
    DriverDirs(elevated bool) ([]string, []error) // elevated: protected folders only
}
```

| Part | Windows | Linux, macOS (today) |
|---|---|---|
| Capture | `capture.WinDivert` (64-bit only; `ErrNoDriver` on arm64) | `capture.Unavailable`: route mode says "packet capture isn't available on this system yet" (TUN backends: #41, #42) |
| Owners | `owner.Windows`: `GetExtendedUdpTable(UDP_TABLE_OWNER_PID)` + Toolhelp32 | Linux `owner.Procfs`: `/proc/net/udp{,6}` inodes matched to `/proc/<pid>/fd` `socket:[inode]` links. macOS `owner.Darwin`: `kern.proc.all`, then `proc_pidinfo(PROC_PIDLISTFDS)` + `proc_pidfdinfo(PROC_PIDFDSOCKETINFO)` through the `proc_info` syscall (no cgo). Own user's processes only, no root. Other OSes: `owner.Unsupported` |
| Privileges | `platform.UAC`: process token, `ShellExecute "runas"` | `platform.Root`: euid 0; no prompt yet (#44) |
| Games, Icons | `detect.System`: Toolhelp32, Steam under Program Files, exe icons | `detect.System` over `owner.Table` (every name a process goes by: short name, program file, argv[0], so Proton games match their `.exe`); Steam (native, Flatpak, Snap; `~/Library/Application Support/Steam`), Wine prefixes (Proton `compatdata`, Lutris, Heroic, Bottles, CrossOver, Whisky, `~/.wine`), Legendary installs; no icons yet |
| Paths | `%APPDATA%\Lowpath`, `%LOCALAPPDATA%\Lowpath`; driver in `FOLDERID_ProgramFiles\Lowpath\driver` | `os.UserConfigDir`/`UserCacheDir` + `Lowpath` (XDG, `~/Library/...`); no driver |
| Flows | `detect.KernelNetwork`: ETW real-time session on Microsoft-Windows-Kernel-Network (needs administrator rights) | `ErrUnsupported` (#43) |
| Inventory | `compat.NewSystem`: service manager list (`SC_MANAGER_ENUMERATE_SERVICE`), registry Uninstall and service keys, `GetAdaptersAddresses`, the internet route from `diag.NetStats`; no admin rights | processes, `net.Interfaces` and the route; services and installed programs `ErrUnsupported` (the boosters are Windows programs) |

Rules: a part not built for an OS fails with an error matching
`errors.ErrUnsupported` (`divert.ErrUnsupported` matches it too) and the API
turns that into an explanation (501 `unsupported`, or the capture reason),
never an empty list presented as real. No package-level mutable state: each
implementation is a type with a constructor, and its dependencies (owner
resolver, privilege check, driver folders) are passed in.

`internal/owner` is the socket-owner resolver:

```go
type Socket struct{ Local netip.AddrPort; PID uint32 }
type Resolver interface {
    UDPSockets() ([]Socket, error)            // IPv4 and IPv6, no admin rights
    ProcessNames() (map[uint32]string, error) // PID -> image name
}
func ListProcesses(r Resolver, self uint32) ([]Process, error) // "Add game" list; portable

type Program struct{ PID uint32; Name string; Names []string; Exe string }
type Table interface{ Resolver; Programs() ([]Program, error) } // Linux, macOS
```

On Linux and macOS a process's short name is cut to 15 or 16 bytes, and a
Windows game under Wine or Proton runs from the Wine loader with its
Windows path in argv[0] (`C:\...\RocketLeague.exe`). `Program.Names` holds
all of them, `Name` is the game's own (argv[0]'s file for Wine, else the
program file, else the short name), and `Exe` maps a Wine argv[0] through
`$WINEPREFIX/dosdevices`. The catalog's install paths are Windows paths;
`detect.Library.Has` looks them up case-insensitively in each prefix's
`drive_c` and matches their last folder against launcher installs. The
`/proc` reader takes an `fs.FS` and the libproc structures are decoded by
portable functions, so both are tested on every OS against fixtures.

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
// backend.go: the contract the app uses
type Backend interface {
    Name() string                      // "divert", "none"
    Available() error                  // cheap; ErrNeedAdmin / ErrNoDriver / errors.ErrUnsupported
    Start(cfg Config) (Session, error) // fills in DriverDir and Owners itself
}
type Session interface { Inject(...) error; Stats() Stats; Close() error }
func NewWinDivert(owners owner.Resolver, priv Elevation, dirs DriverLocations) *WinDivert
func NewUnavailable(err error) *Unavailable

type Config struct {
    DriverDir string               // where divert.Install put the files
    Processes []string             // image names to capture, case-insensitive ("cs2.exe")
    PIDs      []uint32             // extra exact PIDs to capture (optional)
    Exclude   []netip.AddrPort     // relay endpoints: never captured
    Owners    owner.Resolver       // who owns each local UDP socket (required)
    OnPacket  func(local, remote netip.AddrPort, payload []byte) bool
        // called for every captured game packet; payload is only valid during
        // the call. true = consumed (tunneled or dropped by kill switch);
        // false = reinject the original packet unchanged (goes out directly).
    Log *slog.Logger
}
func Start(cfg Config) (*Capture, error) // the WinDivert loop; used by WinDivert.Start
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
  local UDP port → PID through `Config.Owners` (on Windows
  `GetExtendedUdpTable(UDP_TABLE_OWNER_PID)` and a Toolhelp32 snapshot). Cache port→verdict; refresh the game's
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

### Fail-open (C5)

A capture that stops working must never leave the game (or the rest of the
PC's UDP) stuck in the driver's queue. Three layers make sure it doesn't:

- **The OS.** When the process ends for any reason (`taskkill /F`, a
  crash, a Go panic) Windows closes its handles; WinDivert's cleanup
  callback then reinjects the packets still queued and its destroy
  callback deletes the handle's WFP filters, so matching traffic goes out
  directly again with nothing left behind. The driver service itself is
  removed by WinDivert at the next reboot.
- **The capture watchdog**, inside `capture.Capture`. Each receive worker
  flips a counter when it starts and finishes a packet (two atomic adds, no
  clock in the hot path); a watchdog goroutine checks every 100 ms and,
  when one packet has been in the handler for `Config.StallLimit` (500 ms),
  stops the capture: shut down receiving, wait at most 200 ms for the
  workers, close the handle. A panic in the packet path (the engine's
  `Send`, owner lookup) is recovered and does the same, and so do 100
  receive errors in a row (driver stopped). Worst case from a stuck engine
  to direct traffic: about 0.8 s. A tick that arrives late (the PC slept)
  restarts the clock instead of counting the sleep. A plain `Close` (Stop)
  waits up to 3 s for workers that are still draining the queue, but stops
  waiting once one has held the same packet for 200 ms: packets older than
  WinDivert's 2 s queue time are dropped, not sent on, when the handle
  closes.
- **The app.** `Session.Done()` closes when the capture has stopped, and
  `Session.Err()` says why when it stopped on its own. The app then ends the
  session (stopping the engine with a 2 s bound, since a stuck engine may
  never return), and the live view's `failsafe` says what happened.

Leftovers from a run that never got to clean up are handled at the next
start: the app keeps `<data>\capture-session.json` (`capture.MarkerFile`:
backend, PID, image name, game, start time) while a route-mode session runs
and removes it on Stop. If the file is there at start and its PID is not a
running process of that name, the app calls `Backend.Recover(marker)`,
removes the file and shows a notice. WinDivert has nothing to undo (see
above); TUN routes and WFP filters from future backends are undone there. A
marker whose process is still running is left alone. A Go crash also writes
its stack to `<data>\crash.log` (`debug.SetCrashOutput`).

```go
type Session interface {
    ...
    Done() <-chan struct{} // closed once capture has stopped, by Close or on its own
    Err() error            // why it stopped on its own; nil after Close
}
type Backend interface {
    ...
    Recover(m Marker) error // undo what a session that never closed left behind
}
```

`internal/capture/failopen_windows_test.go` checks the acceptance on the
real driver (administrator only; skipped otherwise): a helper process
captures the test's UDP to 192.0.2.1 and swallows it, the test kills it
(`Process.Kill` is `TerminateProcess`, as `taskkill /F`) or lets its
handler deadlock, and a lower-priority sniffing handle must see the packets
leave within 1 s.

### Conflicts with other boosters and VPNs (C7) — `internal/compat`

Two programs rerouting the same packets fight over them, and a silent
capture failure next to ExitLag looks like Lowpath's fault. Lowpath never
fights another driver for the game's packets: it names what is in the way
and says what to do.

`compat.Detector.Scan(ctx)` reads a `compat.System` (the platform's
`Inventory`) part by part and returns a `Report{at, conflicts, unchecked}`:

| What | Found from | Level |
|---|---|---|
| Game boosters: ExitLag, NoPing, WTFast, Mudfish, LagoFast, GearUP, Haste, Pingzapper, Battleping, Kill Ping, Outfox | process name (normalized: "ExitLag.exe" contains "exitlag"; short names such as `haste` match whole names only), service name or display name, Uninstall `DisplayName` | process running: `block`; only its service running: `warn`; installed: `info` |
| WinDivert tools: clumsy | process | `block` (it degrades packets on purpose) |
| WinDivert tools: GoodbyeDPI, zapret (`winws.exe`) | process, service | `warn` (they change web traffic, not usually game UDP) |
| Windows Packet Filter (WinpkFilter), the NDIS filter ExitLag and NoPing install | service `ndisrd`, Uninstall entry | loaded: `warn`; installed: `info`, with how to uninstall it |
| WinDivert loaded by another program | a running service named `WinDivert*` whose `ImagePath` is outside Lowpath's driver folders (`Paths.DriverDirs`) | `warn` |
| A VPN carrying all traffic | the adapter that carries traffic to a public address (Windows: `GetBestInterfaceEx` through `diag.NetStats`; Linux and macOS: `compat.EgressRoute`, the interface owning the source address the kernel picks for a connected UDP socket, which sends nothing, falling back to the `0/0` route) is a VPN; this catches `0/0`, OpenVPN/WireGuard `0/1`+`128/1` and wg-quick's policy routing alike is a VPN: brand words in its name or description (NordLynx → NordVPN, TAP-Windows → OpenVPN, WireGuard, AnyConnect, ...), IANA type PPP/virtual/tunnel, or `tun*`/`wg*`/`utun*` on Linux and macOS; PPPoE, Teredo and ISATAP are not VPNs | `warn`; a split tunnel or a VPN that isn't up is no conflict |

Each conflict carries `found` (the evidence), `action` (one sentence) and
`why` (the "Why?" text). A part that can't be read goes to `unchecked` with
the reason; a part the OS doesn't have (`errors.ErrUnsupported`) is skipped
quietly; offline is no failure.

In the app (`internal/app/conflicts.go`) a `conflictWatch` scans every 15 s
and on demand, one scan at a time; callers wait at most 3 s and otherwise
get the last report with a note, and a panic in an OS call becomes an
`unchecked` line. A missing or stale report never blocks anything (fail
open): Boost is refused only on a scan that finished while it waited. An OS
call that never returns can't be ended; the 30 s limit only cuts the
remaining steps short, and the next scan starts once the call returns.
`POST /api/apply` in route mode checks first, before the rights check,
since restarting as administrator wouldn't help, and answers 409
`conflict` with the blocking conflicts unless `ignore_conflicts` is set
(the UI's "Boost anyway"). Warnings and proxy mode never block. A capture
that fails to start names the running conflicts in its message.

`lowpath -compat-inventory file.json` reads a `compat.Inventory` from a
file instead of the system (read again at every scan, missing file = empty
and offline), so the end-to-end test plays "ExitLag is running" on any PC.

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
  UDP; it ignores `HTTP(S)_PROXY`, which would report the proxy's address). A public address goes to the `NetworkLookup`: RIPEstat
  `network-info` (prefix, origin ASNs), then `as-overview` (holder, split
  into handle and ISP name) and `maxmind-geo-lite` (country, city) in
  parallel; those two may fail alone (`partial`). `CachedLookup` keeps an
  answer per address for 24 h, a partial one 10 min, a failure 30 s, and a
  failure or missing part caused by a timeout or a busy server 3 s.
  Private, CGNAT and loopback answers are never sent to the registry.
- RIPEstat's proxy sometimes holds a request for 5 to 20 s and then
  answers 502, while the same query sent again answers in 0.4 s (its
  `process_time` is about 10 ms either way). So each registry call is
  hedged ("The Tail at Scale"; gRPC's `hedgingDelay`): a second copy goes
  out on a fresh connection after 2.5 s without a reply, or at once after a
  timeout, reset or HTTP 500/502/503/504, and the first good reply wins.
  At most two copies; a 429, an error status, a certificate that does not
  verify or a DNS "no such host" is never repeated; at most 6 requests in
  flight (RIPEstat allows 8 per address) for the whole app, since the
  Network card and the Current server card share one client, and a hedge
  is skipped when none is free. `whats-my-ip` is not hedged.
- One lookup runs at a time, with a 15 s budget that does not depend on a
  caller who gives up; a result is reused for 2 min (an offline or partial
  one for 15 s, one spoiled by a slow or busy registry 4 s) unless
  `refresh=1`.
  States: `ok`; `partial` (IP known, ISP not: `message` says why);
  `offline` (no IP: `message` says why). No IPv6 is normal and only sets
  `ipv6_note`. `retry_after_s` is set when the ISP is unknown only because
  the registry was slow or busy; the Network pane then asks once more by
  itself after that wait (without `refresh`), and says "Trying again…".
- Lowpath never captures its own process, so this is the user's own line
  even while a game is routed through a relay.
- `/api/diagnostics` includes the last location with addresses cut to /24
  (IPv4) and /48 (IPv6), also inside the error texts; it never starts a
  lookup. Lookup errors never repeat the request URL (its query holds the
  address).
- `/api/diagnostics` also carries the current `verdict` (a `Diagnosis`,
  built from the redacted trace).

## Server catalog — `internal/catalog`

Version 1 knows the servers of Valve's SDR games (CS2 730, Dota 2 570,
Deadlock 1422450; `games.json` marks them `"discovery": ["sdr"]`) before
the game starts.

```go
func ParseSDRConfig(appID int, b []byte) (*SDRConfig, error) // SDRConfig{AppID, Revision, POPs []POP}
func Sanitize(pops []POP)                                     // fixes POP.Geo, sets POP.GeoFix
func NewSDRSource(client HTTPDoer, base string) *SDRSource    // Fetch(ctx, appID) ([]byte, error)
func NewPOPProber(p Pinger, now func() time.Time) *POPProber  // Probe(ctx, pops, done func(id, POPPing))
func NewDirCache(dir string) *DirCache                        // Load/Save(key) of the last good download
func NewValveServers(ValveOptions) *ValveServers              // List(appID), Ping(appID), Close()
```

- **Source:** `GET https://api.steampowered.com/ISteamApps/GetSDRConfig/v1/?appid=N`
  (no key). 15 s timeout, 4 MiB cap, `{"success":false}` = no list. The
  reply must parse before it replaces anything.
- **Parse:** POP ids `[a-z0-9]{2,12}`, at most 400 POPs and 64 relays each;
  relay addresses must pass `relay.IsPublic` (a hostile reply can't make
  the client ping its own LAN); invalid port ranges are dropped; the desc
  "City (Country)" is split for display.
- **Geo:** Valve's `geo` is `[lon, lat]`. Each POP is checked against the
  city its id/alias names (a table of metro coordinates, used only when the
  city also appears in the desc), or another POP in the same city: within
  300 km it stays, swapped within 300 km it is swapped (`dfra`, `dvie`,
  `dlhr`), otherwise the city's point replaces it (`mad` has a flipped
  longitude, `dsto` sits on Moscow). With no reference only impossible
  values are fixed.
- **Ping:** ICMP echo (`internal/icmp`, no admin on Windows) to up to two
  relays per POP, two requests each, 1.5 s timeout, 8 POPs at a time; the
  best reply is the POP's ping. No reply → `null` plus a note; a ping that
  can't be sent (permission, IPv6) is not counted as loss.
- **Refresh:** `List` never blocks: it answers from memory and starts a
  download when the list is missing or older than 1 h (backoff 30 s → 15
  min on failure) and a ping round when pings are older than 5 min. The
  last good download is kept in `<data>\catalog\sdr-<app>.json`, shown
  (marked `stale`) when Steam can't be reached. A saved time in the future
  (wrong clock) counts as old. `Ping` without a list retries the download
  at once (the card's "Try again"). A new list that lands during a ping
  round gets its own round when that one ends. A 200 reply that isn't a
  config (Wi-Fi login page, proxy) is reported as such.

## Current server — `internal/app/current.go`

The game page names the server a game is talking to, for every game,
catalog entry or not:

- **Where it comes from**, best first: a running session's flow table
  (`source: "boost"`); otherwise the OS's flow events (`"traffic"`); otherwise
  the last server saved for the game (`"history"`, state `last`). Sessions in
  route mode and the flow watch both save the primary every 5 s (the watch
  only a UDP primary that sent ≥ 10 packets: the saved server is proxy
  mode's endpoint and Analyze's baseline).
- **Flow events** (Windows): an ETW real-time session ("Lowpath Flows") on
  Microsoft-Windows-Kernel-Network `{7dd42a49-5329-4832-8dfd-43d979153a88}`,
  keywords IPv4|IPv6, events 10/11/26/27 (TCP data sent/received) and
  42/43/58/59 (UDP). User data starts `PID u32, size u32, daddr, saddr,
  dport u16, sport u16` (addresses u32 or 16 bytes, all network byte order).
  No packet driver, so it also works next to anti-cheats that refuse
  WinDivert, and it can't break networking. The session opens while a known
  game runs and closes 30 s after the last one exits; a session left by a
  crashed Lowpath is stopped and replaced. Unelevated it fails with
  `ERROR_ACCESS_DENIED` (`detect.ErrNeedAdmin`), retried every 5 min. A
  session stopped from outside closes `FlowWatch.Done()`; the card then
  shows `watch.state: "error"` and the watch is retried after 30 s. The
  callback only looks the PID up in a map of game PIDs (refreshed every 2 s
  from `Owners.ProcessNames`) and drops the rest.
- **Learning** (`engine.FlowLearner`): the same rules as a session (primary
  = most upstream packets over 5 s, UDP before TCP, ties keep the current
  one, forgotten after 60 s idle). Only sends create servers; a receive
  whose ends are reversed is matched to a known server. Ignored: ports 53,
  80, 443 (also QUIC), 853, 1900, 5353, 5355, loopback, link-local,
  multicast, broadcast.
- **Network**: RIPEstat (`network-info`, `as-overview`, `maxmind-geo-lite`),
  cached 24 h, looked up in the background; private, CGNAT and loopback
  addresses are never sent. `NetworkInfo` gains `lat`/`lon` when GeoLite2
  names a city (a country-only answer's point is its center, useless here).
  The diagnostics report drops them (`Location.Redacted`).
- **City check** (`catalog.CheckGeo`): light in fibre covers ≈ 204 km/ms
  (c / 1.468), so a fastest ping of r ms puts the server within
  r / 2 × 204 km. A database city farther than that plus 250 km (city-level
  geolocation error at both ends) is marked `too_far` ("can't be right");
  with no ping or no city, `unknown`.
- **Ping** (`catalog.LivePinger`): ICMP once a second while the card polls
  (stops 15 s after the last poll), last 30 results: mean, min, standard
  deviation (jitter, as in the routes table), loss. A server that never
  answers gets "—" and "doesn't answer pings", not 100 % loss; IPv6 and
  "can't send" are notes, never loss.

| Method | Path | Response |
|---|---|---|
| GET | /api/games/{id}/current-server | `{state: "live"\|"last"\|"none", source, message, watch: {state: "on"\|"idle"\|"needs_admin"\|"unsupported"\|"error", message}, server: null \| {addr, proto, pkts_up, pkts_down, last_seen, network: NetworkInfo\|null, network_note, looking_up, lan, geo: {state, distance_km, max_km, note}, ping_ms, jitter_ms, loss_pct, sent, received, ping_note}, others: [{addr, proto, pkts_up, pkts_down}], you: {city, country}\|null}` |

The snapshot's `servers` entries gain `proto`.

## Diagnostics — `internal/diag`

Measures the user's own connection. Nothing needs admin rights, and every
number can be unknown (`null` in JSON, "—" in the UI) instead of guessed.

```go
type NetStats interface {               // per OS, behind diag.NewNetStats()
    DefaultRoute() (Route, error)       // Route{Index, Name, Gateway netip.Addr, Kind "wifi"|"ethernet"|"other"|""}
    Counters(index int) (Counters, error) // cumulative Rx/TxBytes + link speeds
}
func Rate(prev, cur Counters) (Throughput, bool) // copes with 32-bit wraps
type Prober interface { Name() string; Probe(ctx) (time.Duration, error) } // ICMPProber, TCPProber, FuncProber
type Loader interface { Download(ctx, add func(int64)) error; Upload(...) error; Endpoint() string }
func NewHTTPLoader(HTTPLoaderConfig) (*HTTPLoader, error) // speed.cloudflare.com by default
func NewBloatTest(BloatConfig) *BloatTest; (*BloatTest).Run(ctx, progress) (BloatResult, error)
func NewPassiveDetector(PassiveConfig, LineRate) *PassiveDetector; (*PassiveDetector).Observe(PassiveSample) PassiveState
type TTLPingFunc func(dst netip.Addr, ttl int, timeout time.Duration) (icmp.Reply, error) // icmp.PingTTL
func NewTrace(TraceConfig) *Trace; (*Trace).Run(ctx, progress) (TraceResult, error)
func (TraceResult) Report() string; (TraceResult) Redacted() TraceResult
func NewVerdictEngine(VerdictConfig) *VerdictEngine; (*VerdictEngine).Diagnose(Evidence) Diagnosis
func (DiagnosisReport) Text() string
```

- **NetStats.** Windows: `GetBestInterfaceEx` (toward 1.1.1.1, else
  2606:4700:4700::1111; nothing is sent) picks the interface,
  `GetIpForwardTable2` its default gateway (IPv4 preferred, lowest metric),
  `GetIfEntry2Ex` the 64-bit `InOctets`/`OutOctets`. Linux: `/proc/net/route`
  (then `/proc/net/ipv6_route`) and `/sys/class/net/<if>/statistics`, read
  through an `fs.FS` so the parser is tested on every OS. macOS: routing
  sysctl `NET_RT_DUMP` and `NET_RT_IFLIST2` (`if_data64`). Other OSes:
  `ErrUnsupported`.
- **Bufferbloat test** (on demand, 20 s): 5 s idle, 5 s download, 5 s upload,
  5 s recovery; 10 Hz probes to an internet target and to the gateway, 1 s
  timeout (later = lost). The internet target is picked first (3 probes
  each, ≤ 2 s): the fastest answering of the user's relays (overlay probe;
  loopback and LAN relays skipped), else ICMP to 1.1.1.1, else a TCP
  handshake with the load server (its first IPv4 and first IPv6 address,
  so IPv6-only networks still get a target). Load: 6 parallel HTTP/1.1
  downloads (`GET /__down?bytes=25e6`) and 4 uploads (`POST /__up`,
  10 MB), one TCP connection each, capped at 750 MB down / 250 MB up per
  phase; a server that refuses every transfer (429, 403) counts as not
  loaded, even when upload bytes left before the refusal. The first 1 s of
  a loaded phase is not graded (TCP ramp-up). Increase = loaded p50 −
  idle p50 per direction; grade from the worse direction: A+ < 5 ms, A < 30,
  B < 60, C < 200, D < 400, else F. Half the pings lost under load counts as
  ≥ the timeout. A direction that couldn't be loaded, or no target, gives no
  grade (or a grade from the other direction) and a `note` saying why. Line
  rate per direction = max(adapter counters, the test's own bytes).
- **Passive detector** (1 Hz during a session): fires when the relay leg's
  RTT (`after.rtt_ms − after.server_ms`) is ≥ 20 ms above its baseline
  (minimum over 2 min of seconds when the line wasn't full) while non-game
  traffic (adapter bytes − the engine's `wire_bytes_*`) is ≥ 70% of the line
  rate (the last test; the peak seen only when there was none, since the
  adapter counters also carry LAN traffic) and ≥ 2 Mbit/s, for 3 of 5
  seconds. Clears after 5 clean seconds.
- The app keeps the last result in `state.json` (`bloat`), refuses the test
  during a session (409 `session_active`) and stops a running test when a
  boost starts.
- **Route trace (MTR)** (on demand, 200 rounds at 1 Hz, about 3½ min):
  `icmp.PingTTL` sends a 64-byte echo with TTL 1 to 30 (WinMTR's defaults),
  one goroutine per TTL, staggered over half the interval, 2 s timeout.
  Windows: `IcmpSendEcho2Ex` / `Icmp6SendEcho2` with
  `IP_OPTION_INFORMATION.Ttl`, no admin rights; the reply status says who
  answered (`IP_SUCCESS` = the destination, `IP_TTL_EXPIRED_TRANSIT` /
  `IP_HOP_LIMIT_EXCEEDED` = a router, unreachable codes). Linux and macOS:
  a raw ICMP socket (root or `CAP_NET_RAW`), matching "time exceeded" and
  "unreachable" replies to the probe by the echo ID and sequence quoted
  inside them; without privileges the trace says so. TTLs past the one the
  destination answered at stop; silent trailing hops are left out. Per hop:
  sent, received, raw loss (as WinMTR counts it), best/avg/worst/last/stdev,
  every address that answered (load balancing), reverse DNS.
  **Persistence rule:** a hop's *persisting* loss is the lowest loss at it
  or any later hop up to the destination (`persist_pct`); loss starts at
  the first hop where it reaches the threshold (1 point, or 2 probes when
  fewer than 200 were sent); a hop that loses more than persists is
  `limited` (it rate-limits replies, as Linux routers do at about one per
  second per destination), and a rise seen at a limited hop is credited to
  the next hop whose own numbers match. The same rule on average delay
  (`persist_ms`) finds the hop that adds the most delay; a hop that only
  answers slowly is skipped. With no answer from the destination nothing
  is confirmed (`persist_pct: null`). **UDP mode** (relays only): the hops
  as above, plus a last row of overlay probes to the relay's own UDP port,
  which becomes the destination for the rule. Windows delivers no ICMP
  errors for UDP sockets to unprivileged programs, so the routers are found
  with ICMP.
- The app keeps the last trace in `state.json` (`trace`), refuses it during
  the bufferbloat test and the other way round (409 `bloat_running`,
  `trace_running`), allows it during a boost (about 2 kB/s of probes), and
  puts it in the diagnostics report with addresses cut to /24 and /48.
- **Verdict engine** (`verdict*.go`, roadmap §3.6): `Diagnose` turns
  `Evidence` (connection in use, Wi-Fi signal, gateway pings, the last
  bufferbloat test and trace, the passive detector, a running boost and
  the last route analysis; every field optional) into a `Diagnosis`: one
  `Check` per rule (`found` / `clear` / `unknown` with a summary and the
  measured facts) and the `Verdict` of the first rule that found its
  problem, with a confidence (high / medium / low), the segment it blames
  and whether a relay can help. Rules, in order:
  1. *Home / Wi-Fi*: router loss > 0.5% (at least 2 pings) or p99 > 15 ms
     (a p95 above 15 ms implies it), or a trace whose persisting loss starts
     at hop 1 or a private hop 2 (not one in 100.64.0.0/10: that is the
     ISP's carrier-grade NAT, RFC 6598). On Wi-Fi, RSSI < −70 dBm or 2.4 GHz makes
     it `wifi` (high); a strong 5/6 GHz signal makes it `home`; no signal
     reading caps it at medium. Slow replies with no loss drop a step
     (routers answer ping at low priority). A router that answers no ping
     at all is not loss.
  2. *Overloaded line*: grade C or worse, or the passive flag (high).
  3. *ISP route*: a reached trace whose persisting loss starts past the
     home network (destination-only loss is not the route's); or, from a
     boost, direct loss ≥ 2% while the relay leg loses < 1% and the relay
     reaches the server and its leg to the server isn't lossy. During a
     boost the advice is to keep it on. A UDP trace losing only at the relay's port gives
     `relay` (segment `lowpath`).
  4. *Game server*: the relay leg is clean (< 1% loss, < 10 ms jitter) and
     the relay's leg to the server loses ≥ 2% or jitters ≥ 10 ms (relays
     don't report that leg's loss yet), or, low confidence, during a boost
     direct loses ≥ 2% and the relay reports no time to the server (a hint:
     the relay may not be able to ping, and an ISP problem looks the same).
  5. *Already optimal*: direct RTT ≤ relay route + 2 ms and no lossier
     (≥ 20 samples each); medium while the two are measured differently
     (ICMP vs UDP probes), high once they are the same method.
  No rule found anything: `clean` (high with all four problem checks
  clear, medium with two or three, low with one) or `unknown`, plus `next`
  steps for the unmeasured checks. Evidence older than 1 h drops a step and
  older than 24 h is ignored; a bufferbloat test or route trace from another
  adapter or link kind is ignored (traces record the connection they ran
  on). With no default route at all (`ErrNoRoute`) the verdict is
  `offline` (high), ahead of every rule. The app builds the evidence from what it stored
  (the trace already redacted) and measures nothing itself, so
  `GET /api/diag/verdict` answers at once; `GET /api/diag/verdict/report`
  is `DiagnosisReport.Text()` with the redacted location, for sharing.

## App, desktop shell, build

- `cmd/lowpath` replaces `cmd/lowpathd`. Flags: `-addr 127.0.0.1:7878`,
  `-data`, `-demo`, `-relays file.json` (optional import), `-headless` (no
  window), `-open` (browser instead of window), `-no-elevate`.
- Elevation: bind the port first (if another copy serves it, focus that one
  and exit, no UAC prompt); then, if `Platform.ElevationHelps()` (the
  system can prompt, the process isn't elevated, and capture's only problem
  is `ErrNeedAdmin`: a prompt can't fix a missing driver) and not
  `-no-elevate`, `Privileges.Relaunch` (on Windows `runas` through
  ShellExecuteW) passing `-wait-pid <self>`; on UAC
  cancel keep running unelevated (route mode shows "needs admin").
  `POST /api/elevate` does the same on demand; the child waits for the parent
  to exit before binding.
- Window: `github.com/jchv/go-webview2` (pure Go), title "Lowpath",
  1100×720, min 920×600, icon from resources. Closing the window shuts down
  cleanly (stop session → capture closed → networking restored). Logs go to
  `<data>\lowpath.log` and stderr.
- Resources: app icon + manifest (asInvoker, DPI aware) via `go-winres` →
  `cmd/lowpath/rsrc_windows_amd64.syso`.
- WinDivert files extracted to the first of `Paths.DriverDirs` that works:
  `<Program Files>\Lowpath\driver`, the folder resolved with `KnownFolderPath(FOLDERID_ProgramFiles)` (not the
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
"platform": {"os": "windows", "admin": true, "can_elevate": false,
             "capture": {"backend": "divert", "available": true, "reason": ""}},
"relays": [RelayView]
```
`can_elevate` is true only when `POST /api/elevate` is wired, the process
isn't elevated and capture reports `ErrNeedAdmin`, so the banner never offers
a restart that can't help.
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
| GET | /api/location[?refresh=1] | | `{state, message, at, ipv4, ipv6, ipv6_note, retry_after_s}`; each address `{ip, source, asn, asns, holder, as_name, isp, prefix, country, city, partial, lookup_error}` or null. 504 `location_timeout` |
| GET | /api/conflicts[?fresh=1] | | `{at, conflicts: [{id, name, kind: "booster"\|"packet_tool"\|"packet_filter"\|"vpn", level: "block"\|"warn"\|"info", found, action, why}], unchecked}`, worst first; `fresh=1` scans again (waits at most 3 s) |
| GET | /api/games/{id}/servers | | `ServerList` (below); starts a download/ping round when due |
| POST | /api/games/{id}/servers/ping | | `ServerList`; pings every POP again now |

`ServerList = {source: "valve_sdr"|"", app_id, state: "unsupported"|"loading"|"ready"|"error",
message, revision, fetched_at, stale, pinging, pinged_at, no_relay, pops: POPView[]}`,
`POPView = {id, city, country, desc, lat, lon, geo_fix: ""|"swapped"|"corrected"|"unknown",
relays, ping_ms|null, loss_pct|null, sent, received, note, pinged_at}`. Rows
come measured first (by ping), then not yet measured, then no reply. The UI
polls while `state` is `loading` or `pinging` is true.
| GET | /api/diag/bloat | | `{running, progress: null \| {phase, elapsed_s, total_s, last_ms, load_mbps, target}, result: null \| BloatResult, error?}` |
| POST | /api/diag/bloat | | 202 + the same; 409 `bloat_running` or `session_active` |
| DELETE | /api/diag/bloat | | stops a running test; the same view |
| GET | /api/diag/trace | | `{running, progress: null \| TraceResult, result: null \| TraceResult, error?, targets: [{id, label, addr, udp, kind}], defaults: {count, interval_s}}` |
| POST | /api/diag/trace | `{target_id}` or `{host}`, `mode: "icmp"\|"udp"` | 202 + the same; 400 `bad_target`; 409 `trace_running` or `bloat_running` |
| DELETE | /api/diag/trace | | stops a running trace and keeps what it measured; the same view |
| GET | /api/diag/trace/report | | the last trace as a text table (`lowpath-route-trace.txt`); 404 `no_trace` |
| GET | /api/diag/verdict | | `Diagnosis{at, verdict: {id: "wifi"\|"home"\|"line"\|"isp"\|"relay"\|"server"\|"optimal"\|"clean"\|"unknown"\|"offline", segment, title, detail, advice, confidence: "high"\|"medium"\|"low", lowpath_helps, facts}, checks: [{rule, name, status: "found"\|"clear"\|"unknown", summary, facts}], link, interface, next: [{id: "bloat"\|"trace"\|"measure", text}]}`; measures nothing, answers at once |
| GET | /api/diag/verdict/report | | the diagnosis as text (`lowpath-diagnosis.txt`), addresses cut to /24 and /48, routers' host names left out |

Live adds `"capture": null | {"mode": "route", "captured", "passed",
"injected", "errors", "game_pids": number[], "last_error": string}` and,
during a session, `"line": PassiveState` (`{active, direction, since,
watching, reason, rtt_ms, baseline_ms, other_down_mbps, other_up_mbps,
line_down_mbps, line_up_mbps, line_source}`). The snapshot adds
`wire_bytes_up` / `wire_bytes_down`. `"failsafe": null | {at, kind:
"stopped"|"recovered", game_id, game, message, detail}` is the last boost
that ended without the user asking (see Capture → Fail-open), until the
next Boost or `DELETE /api/failsafe` (Dismiss), which returns the live view.
`"conflicts": null | Report` is the last conflict scan (as `GET
/api/conflicts`). Session adds `"capture_mode"`. Apply takes
`"ignore_conflicts": bool`.

New error kinds: `needs_admin` (409), `no_relays` (409), `conflict` (409, with
`conflicts`: the programs to close first), `capture_failed` (500, with
`conflicts` when one is running),
`unsupported` (501: route mode, the process list or the restart on a system
without that part yet), `unauthorized` (401).

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
- `live.conflicts` → a banner above every page: running ones in red
  (`block`) or amber (`warn`), "<name> is running." + the action + a
  **Why?** that unfolds the explanation and the evidence; installed-only
  ones as a quiet note that can be dismissed until the list changes. A
  refused Boost says so under the button with **Boost anyway**; it goes
  once the program is closed. Settings → Network: **Other network
  software** lists everything found, what couldn't be checked, and
  **Check again**.
- Optimizing in route mode: "Waiting for game traffic — launch the game now"
  until packets flow; then the **Game servers** list (addr, primary, up/down
  packets) and capture counters. Proxy mode keeps the existing
  "point your game at 127.0.0.1:port" hint.
- Before/after strip: `before_known=false` → "Server ignores ping"; after
  without server leg → "to relay" suffix.
- Library: **Add game** → modal listing running processes with UDP sockets
  (from `/api/processes`), name field, add; custom games get a delete action.
- Demo-only controls (Network lab) only when `demo` is true.
- Every game: a **Current server** card (polls every 2 s): city and
  country (struck through with the reason when the ping rules it out), the
  check's note, address and protocol, AS and ISP, prefix, and ping, jitter
  and loss with "—" and the reason when unknown; "Live" or "Last time"; the
  other servers the game talks to; why the flow watch is off when it is.
- `live.failsafe` (no session running) → a banner above the page: "Boost
  stopped to keep your game connected" or "Your last boost was cut short",
  the service's message and detail, and a Dismiss button that hides it
  at once and calls `DELETE /api/failsafe`, so it stays dismissed after a
  reload and in other windows.
- Valve games (`discovery` has `sdr`): a **Valve servers** card on the game
  page, with or without relays, lists the POPs with city, id, country,
  direct ping and loss (six rows, "Show all"), "—" and the reason when a POP
  doesn't answer, a "Ping again" button ("Try again" when the download
  failed), and when the list was fetched. The card polls every 1.2 s while
  a download or ping round runs, every 10 s after a failed download (so the
  service's retry happens), and every 60 s otherwise (hourly refresh,
  5-minute re-ping).

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

UDP over IPv4 only (TCP/IPv6 go direct). The conflict detector knows the
boosters by name: a renamed or new one is missed until it is added to
`internal/compat/products.go`. Some anti-cheats (Vanguard, FACEIT)
may refuse to run alongside the WinDivert driver. Unsigned exe → SmartScreen
warning. Real benefit needs relays on well-peered servers near the game's
datacenter; the loopback relay only proves the pipeline.
