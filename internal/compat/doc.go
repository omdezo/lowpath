// Package compat finds other software that competes with Lowpath for the
// same packets or routes: other game boosters (ExitLag, NoPing and the
// like), packet filter drivers they leave behind (WinpkFilter, WinDivert),
// and full-tunnel VPNs that take the default route. Lowpath never fights
// another driver for the same packets; it names the conflict and says what
// to close first.
package compat
