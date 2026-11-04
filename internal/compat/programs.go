package compat

import (
	"errors"
	"fmt"
)

// programSource is one place the installed-programs list is read from (an
// Uninstall key in one registry view, on Windows).
type programSource struct {
	// name says which place it is in the report's unchecked list, e.g.
	// `HKLM\…\Uninstall (64-bit view)`.
	name string
	// read returns the display names found there: none and no error when
	// the place doesn't exist, and the names it read along with an error
	// when it could read only part of it.
	read func() ([]string, error)
}

// readPrograms merges the names from every source, each once and in the
// order found. A source that fails doesn't hide the others: the names that
// were read are returned together with an error naming each source that
// couldn't be read (fully), so the report can match what it has and still
// say what it didn't check.
func readPrograms(sources []programSource) ([]string, error) {
	var (
		out  []string
		errs []error
		seen = map[string]bool{}
	)
	for _, src := range sources {
		names, err := src.read()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.name, err))
		}
		for _, n := range names {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out, errors.Join(errs...)
}
