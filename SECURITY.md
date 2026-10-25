# Security policy

Lowpath runs with administrator rights (it loads a packet capture driver),
and its relay runs on public servers, so security reports are taken
seriously.

## Reporting a vulnerability

Please **don't open a public issue**. Use GitHub's
[private vulnerability reporting](https://github.com/omdezo/lowpath/security/advisories/new)
instead. Include what you found, how to reproduce it, and the impact you see.
You'll get an answer as soon as possible, and credit in the fix if you want it.

## In scope

- The desktop app and its local API (`127.0.0.1:7878`): anything that lets
  another program or website control Lowpath or read its data.
- Packet capture: anything that leaks or alters traffic of programs the user
  did not select, or leaves networking broken after Lowpath exits.
- The relay: bypassing the access key, using a relay as an open proxy or
  traffic amplifier, or reaching private networks behind it.
- The build and deploy scripts.

## Design notes

- The local API only answers loopback hosts and requires a per-launch secret
  handed to the app window; other programs on the PC can't drive it.
- Relays drop every packet without the token derived from their key, refuse
  private, loopback and multicast destinations, and cap flows and reply
  targets.
- WinDivert files are written to `%ProgramFiles%\Lowpath\driver` (admin-only)
  and loaded by absolute path.
