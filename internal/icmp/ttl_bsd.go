//go:build !windows && !linux && !darwin

package icmp

// The BSDs have no unprivileged ICMP socket: a TTL-limited echo needs the
// raw one.

const ttlNeeds = "root"

func unprivilegedTTL(bool) ttlSender { return nil }
