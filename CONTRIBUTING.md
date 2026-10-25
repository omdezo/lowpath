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

## Pull request process

Every change, including maintainers' own, goes through a pull request.

1. **Start from an issue.** Comment on it so work isn't duplicated. Small
   fixes can open a PR directly.
2. **Branch** from `main` as `<type>/<short-name>`, for example
   `feat/valve-server-list` or `fix/inject-ipv6`.
3. **Commit** with [Conventional Commits](https://www.conventionalcommits.org/):
   `feat(catalog): …`, `fix(capture): …`, `docs: …`, `chore: …`. Reference the
   issue (`#16`).
4. **Open a draft PR early** using the template. Add the issue's milestone and
   area labels, and write `Closes #N` in the body.
5. **Run the local CI and publish its result** as the `local-ci` check:

   ```powershell
   .\scripts\ci.ps1 -Report          # add -E2E when the UI or app flow changed
   ```

   It refuses to report from a dirty tree, so the check always describes the
   pushed commit. CI runs on contributors' machines; GitHub Actions is
   manual-only.
6. **Mark the PR ready** when the check is green, the template's testing
   section is filled in, and `CHANGELOG.md` has a line under *Unreleased*.
7. **Review.** At least one review by someone other than the author, with
   inline comments for anything to change and a final verdict
   (*Ready to merge* or *Needs changes*). Follow-up work becomes new issues.
8. **Merge** with *Squash and merge* once the review says *Ready to merge* and
   `local-ci` is green on the latest commit. `main` is protected: it only
   accepts pull requests whose `local-ci` check passed.

### Style

- Match the surrounding code: small types and functions with clear
  responsibilities, interfaces at OS and network boundaries, comments that
  explain *why*, `gofmt`'d Go, LF line endings.
- Code must build for Windows, Linux and macOS; OS-specific code goes in
  build-tagged files behind an interface.
- Networking code handles timeouts, offline, IPv4/IPv6, NAT, loss and
  permission errors, and always fails open: Lowpath must never leave a
  player's connection broken.
- Be honest in the UI: never show a number that wasn't measured, and say so
  when something can't be measured.

## Ground rules

- Lowpath has **no accounts, logins, trials or subscriptions**, and never will.
- Don't add telemetry that leaves the user's PC without an explicit opt-in.
- Never inject into game processes; capture happens at the network layer only.

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE).
