package owner

import "os"

// New returns the resolver for this operating system.
func New() Resolver { return NewTable() }

// NewTable returns the resolver with the program list game detection uses.
func NewTable() Table { return NewProcfs(os.DirFS("/")) }
