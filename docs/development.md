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
.\build.ps1 -SkipUI         # keep the UI already in ui\dist
```

Outputs:

| File | What it is |
|---|---|
| `dist\Lowpath.exe` | The desktop app (GUI subsystem, UI and driver embedded) |
| `dist\lpcheck.exe` | STUN check that shows which public IP the internet sees |
| `dist\relay-linux-amd64`, `-arm64` | The relay server |
| `dist\deploy\` | Relay deploy scripts |
| `bin\lowpath.exe` | Console build of the app, for debugging |

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
| `-no-elevate` | Don't ask for administrator rights at start. |
| `-addr 127.0.0.1:7878` | UI/API address (loopback only). |
| `-data DIR` | State and log folder (default `%APPDATA%\Lowpath`). |
| `-relays file.json` | Also use a read-only relay list ([example](../relays.example.json)). |
| `-bloat-server URL` | Load server for the bufferbloat test (default `https://speed.cloudflare.com`; it must answer `GET /__down?bytes=N` and `POST /__up`). |

### Files on disk

| Path | Contents |
|---|---|
| `%APPDATA%\Lowpath\state.json` | Settings, favorites, custom games, relays (with keys), learned servers |
| `%APPDATA%\Lowpath\lowpath.log` | Log (starts over above 5 MB) |
| `%APPDATA%\Lowpath\art\` | Cached game artwork and extracted icons |
| `%APPDATA%\Lowpath\catalog\` | Last downloaded Valve server lists (`sdr-<app id>.json`) |
| `C:\Program Files\Lowpath\driver\` | WinDivert driver files (written when elevated) |
| `%LOCALAPPDATA%\Lowpath\WebView2*` | App window data |

### Local API security

The UI/API port is reachable by every program on the PC, so the API answers
only the app's own window: each launch creates a new secret that the window
receives through a one-time launch link and keeps in an HttpOnly cookie.
Requests must also target a loopback host, and mutating calls carry a custom
header. This is local security, not a login.

## Project layout

```
cmd/lowpath        desktop app: API + embedded UI + window + elevation
cmd/relay          relay server (egress or forwarding hop)
cmd/lpcheck        STUN check for verifying routes
internal/app       game list, local store, relays, sessions, game art, HTTP API
internal/catalog   game server catalog: Valve SDR POP lists, geo cleanup, POP ping
internal/platform  OS layer: capture backend, owners, privileges, detection, icons, paths
internal/platform/fakeplatform  in-memory platform for tests (any OS, no admin)
internal/owner     socket-owner resolver (UDP socket -> PID -> image name)
internal/capture   capture backend contract; per-process UDP capture with WinDivert
internal/divert    pure-Go WinDivert 2.2 bindings + embedded signed driver
internal/engine    client: multipath send, dedup, probes, path selection, fallback
internal/relay     egress relay: token check, dedup, deliver, server-leg ping
internal/proto     overlay framing {type, token, flow, seq, ts, addr}
internal/icmp      ICMP echo (IcmpSendEcho on Windows, raw/ping socket on Linux)
internal/diag      user-side diagnostics: public IP and ISP (STUN + RIPEstat, cached)
internal/stun      STUN Binding codec and client (RFC 5389)
internal/dedup     first-copy-wins sliding window
internal/detect    installed/running games, exe icons, Steam libraries, ETW flow watcher
internal/diag      bufferbloat test, passive detector, default route + NIC counters per OS
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
  and the socket owner table — all without admin rights.
- **Relay:** key check, destination policy, server-leg ping, flow limits,
  forwarding hops.
- **App:** local API guard and per-launch session, relay and custom-game
  validation, error kinds, game art lookup, and a full route-mode session
  through the loopback relay on `fakeplatform`, so the app tests (and
  `cmd/lowpath`'s) run unchanged on Windows, Linux and macOS.
- **Platform:** each OS implementation is complete, and what isn't built
  for an OS yet reports `errors.ErrUnsupported` instead of empty results.
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

`$env:LOWPATH_INTERNET=1` adds tests that tunnel real STUN requests to
Google through a keyed relay and through the engine in capture mode,
download the live SDR configs of CS2, Dota 2 and Deadlock and ping Dubai
and Frankfurt, look up this PC's public IP and ISP with real STUN
servers and RIPEstat, and run a short real bufferbloat test against
Cloudflare (about 100 MB of data). `go test -run TestInternet -v
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
adding a custom game, automatic route measurement, Boost in proxy mode with live
traffic, auto-stop on game exit, and route mode asking for admin rights. From
an **administrator** terminal it also tests route mode for real: WinDivert
captures `lpcheck.exe`, tunnels it through the relay, injects the replies,
and checks that networking is normal after Stop.

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
