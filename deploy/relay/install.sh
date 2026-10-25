#!/bin/sh
# Installs or upgrades the Lowpath relay as the systemd service lowpath-relay.
# Safe to re-run: it replaces the binary, rewrites the unit and restarts.
#
#   sudo sh install.sh --binary ./relay-linux-amd64 [--port 40000] [--key-file FILE | --key KEY]
#   sudo sh install.sh --binary ./relay-linux-amd64 --port 40001 --forward EGRESS_HOST:40000
#
# Without --key/--key-file the existing key in /etc/lowpath-relay.env is kept,
# or a new one is generated (and printed) on first install. --key is visible in
# the process list while this runs; prefer --key-file. A --forward hop has no
# key of its own: the egress it forwards to checks the key.
set -eu

NAME=lowpath-relay
# LOWPATH_INSTALL_ROOT relocates every path; it exists only to test this
# script without touching a real system.
ROOT=${LOWPATH_INSTALL_ROOT:-}
BIN=$ROOT/usr/local/bin/$NAME
UNIT=$ROOT/etc/systemd/system/$NAME.service
ENV_FILE=$ROOT/etc/$NAME.env

BIN_SRC=""
PORT=40000
KEY=""
KEY_SET=0
FORWARD=""

usage() {
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
}

die() {
	echo "install.sh: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--binary) [ $# -ge 2 ] || die "--binary needs a path"; BIN_SRC=$2; shift 2 ;;
	--port) [ $# -ge 2 ] || die "--port needs a number"; PORT=$2; shift 2 ;;
	--forward) [ $# -ge 2 ] || die "--forward needs host:port"; FORWARD=$2; shift 2 ;;
	--key) [ $# -ge 2 ] || die "--key needs a value"; KEY=$2; KEY_SET=1; shift 2 ;;
	--key-file)
		[ $# -ge 2 ] || die "--key-file needs a path"
		[ -r "$2" ] || die "cannot read key file $2"
		KEY=$(tr -d '\r\n' <"$2")
		KEY_SET=1
		shift 2
		;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown argument: $1" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh $0 ...)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"

case "$PORT" in
'' | 0* | *[!0-9]*) die "--port must be a number from 1 to 65535" ;;
esac
[ "$PORT" -le 65535 ] || die "--port must be a number from 1 to 65535"

if [ -n "$FORWARD" ]; then
	printf '%s' "$FORWARD" | grep -Eq '^[][A-Za-z0-9.:-]+:[0-9]{1,5}$' ||
		die "--forward must be host:port"
	[ "$KEY_SET" -eq 0 ] || die "a --forward hop has no key; enter the egress's key in Lowpath"
fi

if [ "$KEY_SET" -eq 1 ]; then
	# The key ends up in a systemd EnvironmentFile; keep it to characters
	# that need no quoting there.
	printf '%s' "$KEY" | grep -Eq '^[A-Za-z0-9._~+/=-]{8,128}$' ||
		die "key must be 8-128 characters of A-Z a-z 0-9 . _ ~ + / = -"
fi

# --- binary ------------------------------------------------------------------
if [ -n "$BIN_SRC" ]; then
	[ -f "$BIN_SRC" ] || die "binary $BIN_SRC not found"
	tmp="$BIN.new"
	install -m 0755 "$BIN_SRC" "$tmp"
	# -h exits 0 on a working binary; a wrong architecture fails to exec.
	if ! "$tmp" -h >/dev/null 2>&1; then
		rm -f "$tmp"
		die "$BIN_SRC does not run on this machine ($(uname -m)); upload the matching relay-linux-<arch>"
	fi
	mv -f "$tmp" "$BIN" # atomic: a running relay keeps its old inode
elif [ ! -x "$BIN" ]; then
	die "no --binary given and $BIN is not installed"
fi

# --- key ---------------------------------------------------------------------
old_umask=$(umask)
umask 077
KEY_NOTE=""
ENV_LINE="EnvironmentFile=$ENV_FILE"
EXEC="$BIN -listen :$PORT -stats 60s"
if [ -n "$FORWARD" ]; then
	KEY_NOTE="none on a forwarding hop: Lowpath uses the egress relay's key"
	ENV_LINE="# forwarding hop: no key (the egress checks it)"
	EXEC="$BIN -listen :$PORT -forward $FORWARD"
elif [ "$KEY_SET" -eq 1 ]; then
	printf 'LOWPATH_RELAY_KEY=%s\n' "$KEY" >"$ENV_FILE"
	KEY_NOTE="set from the key you supplied"
elif [ -f "$ENV_FILE" ] && grep -q '^LOWPATH_RELAY_KEY=..*' "$ENV_FILE"; then
	KEY_NOTE="kept the existing key in $ENV_FILE"
else
	KEY=$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 24)
	printf 'LOWPATH_RELAY_KEY=%s\n' "$KEY" >"$ENV_FILE"
	KEY_NOTE="generated: $KEY"
fi
umask "$old_umask"
if [ -f "$ENV_FILE" ]; then
	chown root:root "$ENV_FILE"
	chmod 0600 "$ENV_FILE"
fi

# --- service -----------------------------------------------------------------
OLD_PORT=""
if [ -f "$UNIT" ]; then
	OLD_PORT=$(sed -n 's/^ExecStart=.* -listen :\([0-9][0-9]*\).*/\1/p' "$UNIT")
fi

cat >"$UNIT" <<UNIT_EOF
[Unit]
Description=Lowpath relay (UDP game traffic egress)
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
# Root only for raw ICMP sockets (pinging game servers); capabilities are
# narrowed to that and binding the port.
User=root
$ENV_LINE
ExecStart=$EXEC
Restart=always
RestartSec=2
CapabilityBoundingSet=CAP_NET_RAW CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT_EOF
chmod 0644 "$UNIT"

systemctl daemon-reload
systemctl enable --quiet "$NAME"
systemctl restart "$NAME"

# --- firewall ----------------------------------------------------------------
FW_NOTE="no ufw/firewalld found: make sure UDP $PORT is open in your provider's firewall"
if command -v ufw >/dev/null 2>&1; then
	if [ -n "$OLD_PORT" ] && [ "$OLD_PORT" != "$PORT" ]; then
		ufw delete allow "$OLD_PORT/udp" >/dev/null 2>&1 || true
	fi
	# Rule comments need ufw 0.35+; fall back to a plain rule on older ones.
	ufw allow "$PORT/udp" comment "$NAME" >/dev/null 2>&1 || ufw allow "$PORT/udp" >/dev/null
	if ufw status | grep -q '^Status: active'; then
		FW_NOTE="ufw: allowed UDP $PORT"
	else
		FW_NOTE="ufw (inactive): added a rule for UDP $PORT for when it is enabled"
	fi
elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
	if [ -n "$OLD_PORT" ] && [ "$OLD_PORT" != "$PORT" ]; then
		firewall-cmd --quiet --permanent --remove-port="$OLD_PORT/udp" || true
	fi
	firewall-cmd --quiet --permanent --add-port="$PORT/udp"
	firewall-cmd --quiet --reload
	FW_NOTE="firewalld: allowed UDP $PORT"
fi

# --- status ------------------------------------------------------------------
sleep 1
if ! systemctl is-active --quiet "$NAME"; then
	journalctl -u "$NAME" -n 30 --no-pager >&2 || true
	die "$NAME failed to start (log above)"
fi
systemctl --no-pager --lines=5 status "$NAME" || true
echo
if [ -n "$FORWARD" ]; then
	echo "lowpath-relay is running on UDP port $PORT, forwarding to $FORWARD"
else
	echo "lowpath-relay is running on UDP port $PORT"
fi
echo "  key:      $KEY_NOTE"
echo "  firewall: $FW_NOTE"
echo "  logs:     journalctl -u $NAME -f"
