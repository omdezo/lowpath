package owner

import (
	"errors"
	"fmt"
)

// Unsupported is the resolver for systems without an implementation (the
// BSDs, for example). Every call says so with an error matching
// errors.ErrUnsupported.
type Unsupported struct{ os string }

// NewUnsupported names the system in its errors.
func NewUnsupported(goos string) *Unsupported { return &Unsupported{os: goos} }

func (u *Unsupported) err(what string) error {
	return fmt.Errorf("owner: %s is not implemented on %s yet: %w", what, u.os, errors.ErrUnsupported)
}

func (u *Unsupported) UDPSockets() ([]Socket, error) { return nil, u.err("listing UDP sockets") }

func (u *Unsupported) ProcessNames() (map[uint32]string, error) {
	return nil, u.err("listing processes")
}

func (u *Unsupported) Programs() ([]Program, error) { return nil, u.err("listing programs") }
