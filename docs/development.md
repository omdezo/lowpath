# Development guide

## Requirements

- Windows 10/11 x64 (the client); any OS can build and test the relay.
- Go — the version in [`go.mod`](../go.mod).
- Node.js 20+ (UI build and the end-to-end test).
- WebView2 runtime (preinstalled on Windows 11) for the app window.

## Build

```powershell
.\build.ps1                 # UI, go vet + go test, then dist\ and bin\lowpath.exe
.\build.ps1 -SkipTests      # faster
.\build.ps1 -SkipUI         # keep the UI already built in ui\dist\app
```

The UI build (`npm run build` in `ui\`) goes to `ui\dist\app`, which git
ignores; the programs embed it from there through `ui.FS`. Until it exists
(a fresh clone) they embed the tracked `ui\dist\placeholder\index.html`
instead, so `go vet`/`go test` and Go builds work without Node.js, and the
window shows a page that says how to build the UI; `lowpath` logs a warning
then, and `build.ps1` warns too.

Outputs:

| File | What it is |
|---|---|
| `dist\Lowpath.exe` | The desktop app (GUI subsystem, UI and driver embedded) |
| `dist\lowpathsvc.exe` | The background service, LowpathSvc (see below) |
| `dist\lowpath-linux-amd64`, `-arm64` | The desktop app for Linux |
| `dist\Lowpath-macos.zip` | `Lowpath.app` for macOS 12+, one universal program for Apple silicon and Intel (`scripts/macapp`) |
| `dist\lpcheck.exe` | STUN check that shows which public IP the internet sees |
| `dist\relay-linux-amd64`, `-arm64` | The relay server |
| `dist\deploy\` | Relay deploy scripts |
| `bin\lowpath.exe` | Console build of the app, for debugging |
| `bin\lowpathsvc.exe` | The service, for the end-to-end test |

## Running while developing

```powershell
bin\lowpath.exe -headless -no-elevate
```

logs to the terminal and prints a one-time launch link. Open it in a browser.
For live UI reloads run `npm run dev` in `ui\` (port 5173, `/api` proxied)
and open the printed link with `localhost:5173` in place of `127.0.0.1:7878`.

`bin\lowpath.exe -demo` runs the whole product against a simulated network
(relays, ISP route, game server and game client) with a *Network lab* to
degrade or cut routes — no servers or admin rights needed.

### Command-line options

| Flag | |
|---|---|
| `-demo` | Simulated relays, game server and game client. |
| `-headless` | No window; stop with Ctrl+C. |
| `-open` | Use the default browser instead of the app window. |
| `-no-elevate` | Don't ask for the rights route mode needs at start (UAC on Windows, polkit on Linux). |
| `-addr 127.0.0.1:7878` | UI/API address (loopback only). |
| `-data DIR` | State folder (default `%APPDATA%\Lowpath`, `~/.config/Lowpath`, `~/Library/Application Support/Lowpath`); the log goes there too when it is given. |
| `-logs DIR` | Log and crash report folder (default: the data folder on Windows, `~/.local/state/Lowpath` on Linux, `~/Library/Logs/Lowpath` on macOS). |
| `-relays file.json` | Also use a read-only relay list ([example](../relays.example.json)). |
| `-bloat-server URL` | Load server for the bufferbloat test (default `https://speed.cloudflare.com`; it must answer `GET /__down?bytes=N` and `POST /__up`). |
| `-trace-count N`, `-trace-interval 200ms` | Shorter route traces, for tests (default 200 probes per hop, one per second). |
| `-wifi-reading file.json` | Play a Wi-Fi adapter from a JSON file (`diag.WiFiReadingFile`: the link, the survey, the scan pause, or an error such as `"disconnected"`) instead of reading or tuning the real one, for tests. |
| `-compat-inventory file.json` | Read the other network software (processes, services, installed programs, adapters, internet route) from a JSON file instead of the system, for tests; see `compat.Inventory`. |
| `-no-service` | Run the engine in this process even when the background service is installed. |
| `-service-pipe NAME` | Use a service run by hand on this pipe (Windows) or socket (elsewhere) instead of the installed one; fails if it doesn't answer. |

### Background service (no administrator prompt)

Testers often drive the PC remotely and can't click a UAC prompt. Install
the service once, from the PC, and Lowpath never asks again, after a
reboot too:

```powershell
dist\lowpathsvc.exe install     # one administrator prompt; copies itself to Program Files
dist\lowpathsvc.exe status      # installed? running? does its pipe answer? (no rights needed)
dist\lowpathsvc.exe uninstall   # one prompt; keeps %ProgramData%\Lowpath
```

The service (`LowpathSvc`, LocalSystem, automatic start) runs the engine and
capture and serves the API on `\\.\pipe\lowpath`. `Lowpath.exe` finds it at
start, starts it if it is stopped (no rights needed), and then only shows the
UI: Settings → Network says "Background service: In use". The first install
copies your settings, relays and custom games; after that the service keeps
its own in `%ProgramData%\Lowpath`, so change relays in the app, not in your
`%APPDATA%` file. Reinstall after rebuilding to update the service; the
window logs a warning when their versions differ.

To work on the service without installing it, run it in a terminal and point
a window at it (no administrator rights; route mode then says it needs them):

```powershell
bin\lowpathsvc.exe run -data .svc -pipe \\.\pipe\lowpath-dev
bin\lowpath.exe -headless -no-elevate -addr 127.0.0.1:7880 -data .ui -service-pipe \\.\pipe\lowpath-dev
```

On Linux, `sudo ./lowpathsvc run` serves on `/run/lowpath/api.sock` (on
macOS `/var/run/lowpath/api.sock`) and
`./lowpath` (window or `-headless`) uses it when it answers, before it
would ask polkit or the administrator dialog for anything; there is no
`install` there yet, and `lowpathsvc install` says so without a prompt.
Like the pipe, the socket only serves accounts signed in at the computer:
root, the service's own account, and accounts with a logind session (on
macOS, a utmpx session). Without logind (WSL without systemd, Alpine) that
is the people's uid range in `/etc/login.defs` (1000 to 60000 by default).
Other accounts, such as `nobody` or a daemon's, get `403 Forbidden`, and
the service logs who they were; their window runs the engine itself.

### Files on disk

| Path | Contents |
|---|---|
| `%APPDATA%\Lowpath\state.json` | Settings, favorites, custom games, relays (with keys), learned servers |
| `%APPDATA%\Lowpath\lowpath.log` | Log (starts over above 5 MB) |
| `%APPDATA%\Lowpath\crash.log` | Go crash reports, one header line per start (starts over above 1 MB) |
| `%APPDATA%\Lowpath\capture-session.json` | Exists only while route mode captures; found at start, it means the last run was killed or crashed mid-boost |
| `%APPDATA%\Lowpath\art\` | Cached game artwork and extracted icons |
| `%APPDATA%\Lowpath\catalog\` | Last downloaded Valve server lists (`sdr-<app id>.json`) |
| `C:\Program Files\Lowpath\driver\` | WinDivert driver files (written when elevated) |
| `%LOCALAPPDATA%\Lowpath\WebView2*` | App window data |
| `C:\Program Files\Lowpath\lowpathsvc.exe` | The installed service program |
| `%ProgramData%\Lowpath\` | The service's `state.json`, `lowpath.log`, `crash.log`, `capture-session.json`, `art\`, `catalog\` (SYSTEM and Administrators only) |

On Linux and macOS the same files are in the per-user folders: settings,
art and server lists in `~/.config/Lowpath` (`$XDG_CONFIG_HOME`) or
`~/Library/Application Support/Lowpath`; `lowpath.log` and `crash.log` in
`~/.local/state/Lowpath` (`$XDG_STATE_HOME`) or `~/Library/Logs/Lowpath`
(Console.app shows them); the window's profile in `~/.cache/Lowpath/window`
or `~/Library/Caches/Lowpath/window` (a Chromium window's is removed when it
closes). On Linux the first windowed start also writes the app menu entry
`~/.local/share/applications/lowpath.desktop` and its icon
`~/.local/share/icons/hicolor/256x256/apps/lowpath.png` (delete both to
remove it), and a running copy listens on
`$XDG_RUNTIME_DIR/lowpath/ui-<address>.sock` for later starts.

### Local API security

The UI/API port is reachable by every program on the PC, so the API answers
only the app's own window: each launch creates a new secret that the window
receives through a one-time launch link and keeps in an HttpOnly cookie.
Requests must also target a loopback host, and mutating calls carry a custom
header. This is local security, not a login. With the background service the
window keeps that guard and the session secret; the service's pipe needs
none, because its access list only admits users signed in at this PC, and
the window checks that the process serving it is the installed service.

## Project layout

```
cmd/lowpath        desktop app: API + embedded UI, wired to the desktop shell; uses the service when it answers
cmd/lowpathsvc     background service LowpathSvc: install, status, run (API on a named pipe, or a Unix socket on Linux and macOS)
cmd/relay          relay server (egress or forwarding hop)
cmd/lpcheck        STUN check for verifying routes
internal/app       game list, local store, relays, sessions, game art, HTTP API
internal/catalog   game server catalog: Valve SDR POP lists, geo cleanup, POP ping
internal/shell     wiring shared by cmd/lowpath and cmd/lowpathsvc: build the server, log, crash log
internal/platform  OS layer: capture backend, owners, privileges, detection, icons, paths, service, pipe, desktop shell (window, single instance, app menu)
internal/platform/fakeplatform  in-memory platform for tests (any OS, no admin)
internal/compat    conflict detector: other boosters, WinpkFilter/WinDivert drivers, full-tunnel VPNs
internal/owner     socket-owner resolver (UDP socket -> PID -> image name)
internal/capture   capture backend contract; per-process UDP capture with WinDivert (Windows) or a TUN device (Linux)
internal/netlink   netlink messages without cgo: rtnl (links, addresses, routes, rules), nft (nftables)
internal/divert    pure-Go WinDivert 2.2 bindings + embedded signed driver
internal/engine    client: multipath send, dedup, probes, path selection, fallback
internal/relay     egress relay: token check, dedup, deliver, server-leg ping
internal/proto     overlay framing {type, token, flow, seq, ts, addr}
internal/icmp      ICMP echo and TTL-limited echo (IP Helper on Windows, raw/ping socket elsewhere)
internal/diag      user-side diagnostics: public IP and ISP (STUN + RIPEstat, cached)
internal/stun      STUN Binding codec and client (RFC 5389)
internal/dedup     first-copy-wins sliding window
internal/detect    installed/running games, exe icons, Steam libraries, ETW flow watcher
internal/diag      gateway pinger, bufferbloat test, passive detector, route trace (MTR), verdict engine + route segments, default route + NIC counters per OS
internal/demo      simulated topology + game traffic generator (-demo)
internal/netsim    UDP hop with delay/jitter/loss/spike impairment
internal/gamesim   echo game server
deploy/            relay install scripts
ui/                React + TypeScript UI (Vite), embedded via go:embed
e2e/               end-to-end UI test (Playwright + Edge)
docs/              architecture, roadmap, guides
```

## Tests

```powershell
go vet ./...
go test ./...
```

- **Engine:** the real client, relay and dedup over lossy simulated links
  (about 9% loss direct vs 0.00% with 3 copies), failover, no route flapping,
  kill switch, capture mode, relay restarts.
- **Capture:** the generated filter against WinDivert's own filter compiler,
  packet parsing/building and checksums byte-for-byte against WinDivert's,
  and the socket owner table — all without admin rights. The fail-open
  watchdog (a stuck or panicking handler, a broken handle, sleep and
  resume) and the session marker file run on a fake handle; from an
  **administrator** terminal `go test -run FailOpen -v ./internal/capture`
  also kills a real capturing process, and lets one deadlock, and measures
  how fast the traffic goes direct (under 1 s).
- **Linux capture (TUN):** the nftables rule set, the netlink messages
  (`internal/netlink`), the rights check and the direct-ports fast path on
  every OS. As **root** on Linux, `go test -run TestTUNRouting -v
  ./internal/capture` runs the real backend in two network namespaces of
  its own (nothing on the machine changes, WSL2 works): capture and
  injected replies, other programs going direct, Stop, a stuck handler,
  `kill -9` as root and with only `CAP_NET_ADMIN` and `CAP_NET_RAW`,
  Recover, no rights, and, with `LOWPATH_LPCHECK` set to a Linux `lpcheck`
  build, lpcheck reporting the relay's address while it is boosted.
- **Relay:** key check, destination policy, server-leg ping, flow limits,
  forwarding hops.
- **App:** local API guard and per-launch session, relay and custom-game
  validation, error kinds, game art lookup, fail-open (the session ends and
  says why; a killed run's marker is cleaned up at the next start), and a
  full route-mode session
  through the loopback relay on `fakeplatform`, so the app tests (and
  `cmd/lowpath`'s) run unchanged on Windows, Linux and macOS.
- **Platform:** each OS implementation is complete, and what isn't built
  for an OS yet reports `errors.ErrUnsupported` instead of empty results.
- **Desktop shell:** on every OS, with programs played by a fake: window
  kinds that fail at start (the next one opens), close, hang on quit
  (killed after SIGTERM) or are opened again by a second start; the
  WebKitGTK, Chromium (snap and not) and WKWebView commands, with the
  launch link only in the environment; the app menu entry and its Exec
  quoting; the polkit prompt dismissed, refused, failing or leaving no
  capabilities, `nosuid` and read-only filesystems; the macOS
  administrator prompt and its quoting; log folders; `Lowpath.app` as a
  universal program with its icon, and its zip keeping the program
  executable. On Linux and macOS the single-instance socket for real
  (a second start, a killed copy's socket, a folder open to others). In
  `cmd/lowpath`, the whole app on `fakeplatform`: closing the window stops
  a route-mode boost and the app, a second start brings the window
  forward with a fresh launch link, and with no window the browser opens
  signed in.
- **Owners and detection on Linux and macOS:** `/proc` fixtures (IPv4,
  IPv6, v4-mapped, a Proton game, another user's process, no `/proc`),
  libproc reply layouts, Wine name and path rules, and a home folder with
  Steam, Proton, Lutris, Heroic and Legendary files, on every OS; on Linux
  and macOS the tests also find their own sockets and process for real.
- **Catalog:** SDR config parsing against a trimmed copy of the live CS2
  config, every geo fix seen in it, hostile replies, download errors and
  timeouts, the disk cache, POP pinging, and the refresh/backoff/offline
  behavior of the server list (fake clock, source and pinger).
- **Diagnostics:** the STUN client against local fake servers (resends,
  stray replies, timeouts, closed ports, cancellation), RIPEstat parsing
  against canned replies, the lookup cache, and every locator state
  (offline, no IPv6, partial, private addresses, shared lookups).
- **Current server:** the Kernel-Network event parser for every flow event
  id, the ETW struct layouts against the SDK sizes, the unelevated
  `ERROR_ACCESS_DENIED` path (as administrator the same test watches its own
  UDP packet to 192.0.2.1), the flow learner's server choice, the city check
  against the speed of light in fibre, the live pinger (loss, jitter,
  unanswered and unsendable pings), and the card's whole flow on
  `fakeplatform`: game starts, flow events, lookup, ping, saving, game exit.
- **Bufferbloat:** grading grading against a simulated line (latency
  that rises under load, lost pings, a refusing load server, no target),
  the HTTP loader against a local speed-test server, target choice, the
  passive detector's rules second by second, `/proc` and sysfs parsing from
  fixtures, and the Windows IP Helper calls without admin rights.
- **Conflicts:** every rule against inventories (ExitLag running, only its
  service, only installed; the other boosters and WinDivert tools by name,
  and names that only look alike; WinpkFilter loaded and left behind;
  WinDivert from Lowpath's folder vs another program's; full, split and
  down VPNs, PPPoE and Teredo, Linux and macOS tunnel names), parts that
  fail on their own, cancellation, the inventory file, redaction; the
  Windows reader without admin rights (the TCP/IP driver, a loopback
  adapter); and in the app, Boost refused next to ExitLag with "Boost
  anyway", the conflict before the admin prompt, warnings and proxy mode
  never blocking, a capture failure naming the conflict, and a hung or
  panicking scan failing open.
- **Router pings:** the gateway pinger stepped on a fake clock: a clean
  router, loss and spikes, one that ignores ping, no rights, no route, no
  router address, a routing table that can't be read, Wi-Fi to Ethernet,
  the bufferbloat test's pause, a boost holding it, idling after the last
  look, a sleep gap, a rate-limiting router (2 Hz fallback), an IPv6
  link-local router, a router so slow the in-flight cap holds; the
  statistics (percentiles, jitter, loss runs, the limiter's shape against
  Wi-Fi bursts), the route segments and the report; and the API on
  `fakeplatform` (nothing pinged until the tab looks, the verdict and the
  export using it, the pause during the bufferbloat test).
- **Route trace:** the persistence rule against a simulated route (clean,
  a router limiting its replies, loss from one hop on, loss at home, only
  the destination losing, a destination that ignores ping, load balancing,
  delay that carries on vs a slow router), UDP mode with a lossy and a dead
  relay, offline and missing privileges, stopping early, the text report,
  redaction, matching routers' replies to probes from raw packets, a real
  TTL-limited echo to 127.0.0.1 and ::1, and the API with the loopback relay.
- **Verdict:** every rule against hand-built evidence (router loss on Wi-Fi
  with and without a signal reading, a cable, one stray lost ping, a router
  that ignores ping, slow replies only, a test from another adapter, old
  and stale evidence, grades A+ to F, the passive flag, loss starting at
  home, past home, only at the destination or only at a relay's UDP port,
  a lossy direct route next to a clean relay, a lossy server leg, already
  optimal with and without the relay's server leg), the order in which
  they win, the text report (no full addresses, 78 columns), and the API
  on `fakeplatform` with stored tests, a learned server and an analysis.
- **Background service:** without admin rights, the real named pipe (many
  connections at once, the access list read back from the pipe: Interactive
  Users can't create instances, nobody remote or anonymous is listed), the
  server-process check refusing another process, a second server refused;
  the Unix socket (stale file taken over, a running one and a plain file
  left alone; the peer check on every OS with the peer played by a fake:
  signed-in, root, `nobody` and an unknown peer, 403 before the API sees
  the request, one log line per account a minute, a client that never
  reads holding nobody up, the window refusing a socket root doesn't
  serve; logind's session files, macOS utmpx records and `login.defs`
  parsed from samples; on Linux and macOS the kernel's peer credentials
  for this process, and as **root** on Linux, `go test -run PeerCheck -v
  ./internal/platform` connects as `nobody` and as uid 1000 to 1002 from
  child processes, against this system's sessions and a sample logind
  list; WSL2 works); the service manager read for a service that isn't installed
  and for the event log, install and uninstall refused without rights, the
  service's and data folder's access lists, the data folder created with
  its access list in one call, only an admins-only folder kept and any
  other moved aside, the service securing its data folder at every start
  before its log, the settings copy never overwriting; the stop handler (Running,
  Interrogate, Stop ends the run, a failing run exits non-zero for the
  restart). In the app on `fakeplatform`: the window's API through the
  service (Boost in route mode starts capture in the service, the session
  cookie never reaches it, refused requests never reach it), the service
  killed (503, the live stream ends) and back, an impostor refused, and
  every start-up case of the window (not installed, stopped and started
  with no prompt, not answering, `-demo`, `-no-service`, `-service-pipe`).
  `lowpathsvc` install relaunching itself elevated with this user's folder,
  the install config and settings copy, uninstall, status, and serving and
  stopping on the API.
- **Wi-Fi:** the Native Wifi structures (realtime connection quality with
  one link and Wi-Fi 7 multi-link, connection attributes, the BSS list
  with its elements after the entries, the interface list) decoded from
  bytes in the 64-bit SDK layout, short and hostile replies, the unit
  rules for rates, frequencies and widths, channels on 2.4, 5 and 6 GHz,
  the BSS Load element, the channel survey (same channel, 2.4 GHz overlap,
  an AP that sends no load or isn't in the scan), `/proc/net/wireless`
  fixtures, a busy channel and a weak signal in the verdict and the
  report's Wi-Fi line; on Windows the real API without admin rights (a
  disconnected or missing adapter gives a reason, and nothing is changed);
  and in the app on `fakeplatform`: every card state, a cable next to a
  connected Wi-Fi, a hung Wi-Fi service (the card and the verdict answer in
  time), the survey with location refused, and the scan pause through a
  route-mode boost: applied, applied again, reverted on stop, off in
  Settings, not on a cable, refused, half applied, and a late answer
  reverted.

`$env:LOWPATH_INTERNET=1` adds tests that tunnel real STUN requests to
Google through a keyed relay and through the engine in capture mode,
download the live SDR configs of CS2, Dota 2 and Deadlock and ping Dubai
and Frankfurt, look up this PC's public IP and ISP with real STUN
servers and RIPEstat, run a short real bufferbloat test against
Cloudflare (about 100 MB of data), trace the route to 1.1.1.1 for 20
rounds, and ping this PC's own router for 8 s with the real route reader. `go test -run TestInternet -v
./internal/diag` prints what they found.

### End-to-end UI test

[`e2e/run.mjs`](../e2e/run.mjs) runs the real app, drives its UI in Microsoft
Edge, sends real traffic through it with `lpcheck`, checks the numbers and
saves a screenshot of every step plus `report.md` to `e2e\out\`:

```powershell
.\build.ps1 -SkipTests
cd e2e; npm install; node run.mjs
```

From a normal terminal it covers the home screen, relay setup, the
bufferbloat test (against a paced local load server started by the test),
a short route trace over UDP to the loopback relay and over ICMP to
Cloudflare with its text report, adding a custom game, automatic route measurement, Boost in proxy mode with live
traffic, auto-stop on game exit, Boost in route mode refused next to a
running ExitLag (played through `-compat-inventory`, so the PC's own
software doesn't matter) with the banner, "Why?" and the Settings list,
the Wi-Fi card (played through `-wifi-reading`: signal, channel, link
rate, *Check channel* and a disconnect) and the scan-pause toggle, and
route mode asking for admin rights. From
an **administrator** terminal it also tests route mode for real: WinDivert
captures `lpcheck.exe`, tunnels it through the relay, injects the replies,
and checks that networking is normal after Stop. Last, it runs
`bin\lowpathsvc.exe run` on a pipe of its own with a second window in front
of it: Settings says the service is in use, Boost goes through the service,
the service is killed (the window says it lost it) and started again (the
window reconnects by itself).

## Verifying route mode by hand

`lpcheck.exe` asks a public STUN server which address it sees (20 requests/s)
and prints RTT, jitter, loss and that public IP.

1. Run `dist\lpcheck.exe` without Lowpath and note the public IP.
2. In Lowpath (as administrator): **Add game** → pick `lpcheck.exe` → select
   it → **Boost**.
3. Run `dist\lpcheck.exe -for 30s`. Through a real relay the public address
   becomes the **relay's IP**; through the loopback test relay the capture
   counters (captured / injected) rise and every reply still arrives.
4. Stop (or just let lpcheck exit), run it again: back to your own IP.

### On Linux

Route mode needs root, or two capabilities on the binary. Started from the
desktop, Lowpath asks for them itself: the polkit password dialog runs
`setcap cap_net_admin,cap_net_raw+ep` on the program file (once; a new
version asks again), and the program starts again as the same user, so its
window and files stay yours. Cancel it and the banner offers **Allow route
mode** later, or proxy mode. Before the prompt, a program file you own is
made `0700`: with the capabilities on it, anyone who can run it could change
the system's routes and firewall. A copy started under `no_new_privs` (a
sandbox, or a systemd unit with `NoNewPrivileges=yes`) isn't offered the
prompt, because the kernel grants no file capabilities there. By hand:

```sh
(cd ui && npm ci && npm run build)   # without it the window only says how to build the UI
go build -o lowpath ./cmd/lowpath && go build -o lpcheck ./cmd/lpcheck
sudo setcap cap_net_admin,cap_net_raw+ep ./lowpath
./lowpath                        # app window; -headless prints the launch link instead
```

The window is a WebKitGTK view run by the system's Python (`python3-gi`
and the WebKitGTK typelib, on Ubuntu `gir1.2-webkit2-4.1`, on Fedora
`webkitgtk6.0` or `webkit2gtk4.1`; GNOME desktops have them), else Chrome,
Chromium, Edge, Brave or Vivaldi in app mode, else the default browser.
Without a graphical session (`WAYLAND_DISPLAY` and `DISPLAY` unset, as over
ssh) no window is tried, and the address is printed instead.
Closing it quits Lowpath and stops the boost. A file downloaded with a
browser isn't executable yet: `chmod +x lowpath-linux-amd64`, or tick
*Allow executing file as program* in its properties, then open it; from
then on it is in the app menu.

Add `lpcheck` as a game (Add game lists it while it runs) and Boost it, then
run `./lpcheck -for 30s`: through a real relay it shows the relay's IP. While
it runs, `ip rule` shows `5100: from all fwmark 0x4c50 lookup 19536` and
`ip link` a `lowpath0` device. After Stop neither is left. After `kill -9`
of Lowpath the device is gone at once, so the rule routes nothing; it is
removed at the next start.

### On macOS

Unzip `Lowpath-macos.zip` and open `Lowpath.app`. It isn't signed yet, so
the first time macOS asks: right-click it, **Open**, then **Open** (on macOS
15, *System Settings → Privacy & Security → Open Anyway*). The UI opens in a
WKWebView window; closing it, or Cmd-Q, quits Lowpath. Route mode isn't
available on macOS yet (#42), so the app offers proxy mode; once it is, the
banner's **Restart with administrator rights** shows the system's password
dialog.
