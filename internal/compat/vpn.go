package compat

import "strings"

// IANA ifTypes that VPN adapters use on Windows: PPP for the built-in VPN
// client (PPTP, L2TP, SSTP, IKEv2), "other virtual" for Wintun (WireGuard
// and most VPN apps) and tunnel for IP-in-IP adapters.
const (
	ifTypePPP     = 23
	ifTypeVirtual = 53
	ifTypeTunnel  = 131
)

// vpnBrands map words in an adapter's name or description to the VPN's
// name. Brands come before protocols: NordVPN's adapter is "NordLynx", a
// WireGuard tunnel, and should be called NordVPN.
var vpnBrands = []struct{ key, name string }{
	{"nordlynx", "NordVPN"}, {"nordvpn", "NordVPN"},
	{"expressvpn", "ExpressVPN"}, {"protonvpn", "Proton VPN"}, {"proton", "Proton VPN"},
	{"mullvad", "Mullvad VPN"}, {"surfshark", "Surfshark"}, {"cyberghost", "CyberGhost"},
	{"windscribe", "Windscribe"}, {"tunnelbear", "TunnelBear"}, {"hotspotshield", "Hotspot Shield"},
	{"privateinternetaccess", "Private Internet Access"}, {"ipvanish", "IPVanish"},
	{"anyconnect", "Cisco AnyConnect"}, {"ciscosecureclient", "Cisco Secure Client"},
	{"fortinet", "FortiClient VPN"}, {"forticlient", "FortiClient VPN"},
	{"globalprotect", "GlobalProtect"}, {"pangp", "GlobalProtect"},
	{"pulsesecure", "Pulse Secure"}, {"juniper", "Juniper VPN"}, {"zscaler", "Zscaler"},
	{"cloudflarewarp", "Cloudflare WARP"}, {"speedify", "Speedify"},
	{"softether", "SoftEther VPN"}, {"tailscale", "Tailscale"}, {"zerotier", "ZeroTier"},
	{"openvpn", "OpenVPN"}, {"tapwindows", "OpenVPN"}, {"tapwin32", "OpenVPN"},
	{"wireguard", "WireGuard"}, {"wintun", "a VPN"},
}

// unixTunnelPrefixes are interface names VPNs create on Linux and macOS
// (tun0, wg0, utun3, ...). Matched as prefixes of the name only: "tap" or
// "wg" inside a longer word means nothing.
var unixTunnelPrefixes = []string{"tun", "tap", "wg", "utun", "ipsec", "nordlynx", "proton", "mullvad", "tailscale"}

// notVPN are virtual adapters that carry no VPN: IPv6 transition tunnels,
// PPPoE (the DSL connection itself is a PPP link) and loopback adapters.
var notVPN = []string{"teredo", "isatap", "6to4", "pppoe", "loopback", "lowpath"}

// vpnName returns the VPN's name when a is a VPN adapter, or "".
func vpnName(a Adapter) string {
	text := normalize(a.Name + " " + a.Description)
	for _, n := range notVPN {
		if strings.Contains(text, n) {
			return ""
		}
	}
	for _, b := range vpnBrands {
		if strings.Contains(text, b.key) {
			return b.name
		}
	}
	name := strings.ToLower(a.Name)
	for _, p := range unixTunnelPrefixes {
		if strings.HasPrefix(name, p) && (len(name) == len(p) || isDigit(name[len(p)])) {
			return "a VPN"
		}
	}
	switch a.Type {
	case ifTypePPP, ifTypeVirtual, ifTypeTunnel:
		return "a VPN"
	}
	return ""
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// adapterLabel is how the adapter is named after a generic "a VPN".
func adapterLabel(a Adapter) string {
	if a.Description != "" && a.Description != a.Name {
		return a.Description + " (" + a.Name + ")"
	}
	return a.Name
}
