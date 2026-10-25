# Contributing to Lowpath

Thanks for helping make online games feel better for everyone. Lowpath is
free and open source, and contributions of every size are welcome: bug
reports, game server data, relay hosting tips, code and docs.

## Ways to help

- **Report lag you can't explain.** Open an issue with your game, region,
  ISP and the diagnostics export. Real-world measurements guide what we build.
- **Add or fix game data.** Server regions, process names and ports for a
  game live in [`internal/app/games.json`](internal/app/games.json).
- **Run a relay** near a game's datacenter and share how it performs
  (see [`deploy/README.md`](deploy/README.md)).
- **Pick an item from the [roadmap](docs/roadmap.md)** and open a pull request.

## Development setup

Requirements: Windows 10/11 x64, Go (version in [`go.mod`](go.mod)) and
Node.js 20+.

```powershell
git clone https://github.com/omdezo/lowpath.git
cd lowpath
.\build.ps1            # UI + app + relays + tests
```

See [docs/development.md](docs/development.md) for the project layout, the
test suites and how to run the app while working on it.

## Pull requests

1. Keep changes focused; one topic per pull request.
2. Run `go vet ./...`, `go test ./...` and `cd ui && npm run build`. For UI
   or app changes also run the end-to-end test (`cd e2e && node run.mjs`).
3. Match the surrounding style: small functions, comments that explain *why*,
   `gofmt`'d Go, LF line endings.
4. Be honest in the UI: never show a number that wasn't measured, and say so
   when something can't be measured.

## Ground rules

- Lowpath has **no accounts, logins, trials or subscriptions**, and never will.
- Don't add telemetry that leaves the user's PC without an explicit opt-in.
- Never inject into game processes; capture happens at the network layer only.

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE).
