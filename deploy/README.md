# Deploying a Lowpath relay

A relay is a small Linux server that receives your game's packets from
Lowpath, drops duplicate copies, sends them to the game server, and sends the
replies back. It only helps when its route to the game server is better than
your ISP's, so **rent it in the same city or datacenter region as the game
servers** (for example Frankfurt for EU CS2/Valorant, Virginia for US East),
with a provider that has good peering. Any 1 vCPU / 512 MB VPS with a public
IPv4 address is enough.

## One command from Windows

Requirements: the server runs a systemd Linux (Ubuntu, Debian, Fedora, ...),
you can `ssh` into it as root or as a user with `sudo`, and you have built
Lowpath (`.\build.ps1` creates `dist\relay-linux-amd64` and `-arm64`).

```powershell
.\deploy\deploy-relay.ps1 -Server root@203.0.113.10
```

It detects the server's architecture, uploads the relay and
`relay/install.sh`, installs the `lowpath-relay` systemd service, opens the
UDP port in ufw or firewalld when present, and prints what to enter in
**Lowpath → Settings → Relays → Add relay**:

```
    Address  203.0.113.10:40000
    Key      kq3VZ0x8...   (24 random characters)
```

Options: `-Port 40001`, `-Key <your own key>`, `-KeepKey` (upgrade the binary
without changing the key), `-Arch amd64|arm64`, `-Binary <path>`,
`-Forward <egress host:port>` (see below), `-PublicHost <ip or name>` (when
`-Server` is an ssh alias). Running it again is safe; without `-Key` or
`-KeepKey` it generates a new key, so update Lowpath afterwards.

Without Windows, copy `relay-linux-<arch>` and `relay/install.sh` to the
server and run `sudo sh install.sh --binary ./relay-linux-amd64 --port 40000`
(it generates and prints a key on first install).

## Ports and firewalls

- The relay uses **one UDP port** (default 40000). Nothing else needs to be
  open besides SSH.
- Cloud firewalls (AWS security groups, GCP/Azure/Oracle network rules,
  Hetzner/Vultr firewalls) are outside the server: allow UDP 40000 there.
- Check from the server: `journalctl -u lowpath-relay -f` shows flows opening
  and a stats line every minute (packets, token drops, policy drops).

## Security

- Always use a key. Without one the relay is an open UDP proxy that anyone can
  use to send traffic that appears to come from your server; it prints a loud
  warning at startup in that case. Every packet without the right key is
  dropped silently.
- The key lives in `/etc/lowpath-relay.env` (mode 0600) and reaches the
  relay through the environment, not the command line.
- The relay refuses to send to loopback, private, link-local, CGNAT,
  multicast and broadcast addresses and to its own IPs, so a leaked key
  cannot be used to reach the server's internal network.
- It runs as root only so it can ping game servers (raw ICMP); the systemd
  unit drops every capability except `CAP_NET_RAW` and
  `CAP_NET_BIND_SERVICE` and makes the filesystem read-only.

## Several routes: forwarding hops

Multipath sends every packet over several routes at once and the **egress**
relay keeps the first copy. That only works if all routes for a game server
end at the **same egress**: two egress relays would each deliver a copy, and
the game server would see your traffic from two IPs. So add extra routes as
*forwarding hops* in front of one egress, ideally on other providers or
carriers:

```powershell
# the egress, near the game server
.\deploy\deploy-relay.ps1 -Server root@egress.example.com
# each extra hop: passes packets to the egress unchanged
.\deploy\deploy-relay.ps1 -Server root@198.51.100.20 -Port 40001 -Forward egress.example.com:40000
```

(By hand that is `lowpath-relay -listen :40000` with `LOWPATH_RELAY_KEY`
set, and `lowpath-relay -listen :40001 -forward egress.example.com:40000`.
The hop resolves the egress name once at startup.)

A hop does not check keys (the egress behind it does) and can only ever reach
that one egress. It passes only packets in Lowpath's format, gives each client
address its own socket toward the egress, and frees it after 60 s of silence
(10 s if the egress never answered, as with a wrong key or spoofed traffic);
it holds at most 4096, and when full it drops an unanswered one for a
newcomer, never a working client. In Lowpath, add each hop as a relay with the **egress's key**
and set "forwards to egress" to the egress relay, so Lowpath treats them as one
group.
