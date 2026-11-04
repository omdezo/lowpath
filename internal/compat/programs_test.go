package compat

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func source(name string, names []string, err error) programSource {
	return programSource{name: name, read: func() ([]string, error) { return names, err }}
}

// One place failing doesn't hide the others: their names are merged once
// each, in order, and the error names the place that failed, including one
// that was only partly read.
func TestReadProgramsOneSourceFailing(t *testing.T) {
	denied := errors.New("Access is denied.")
	cut := errors.New("more data is available")
	for _, c := range []struct {
		name    string
		sources []programSource
		want    []string
		failed  []string // source names the error must mention
	}{
		{
			name:    "all read",
			sources: []programSource{source("A", []string{"ExitLag", "Steam"}, nil), source("B", nil, nil), source("C", []string{"Steam", "Discord"}, nil)},
			want:    []string{"ExitLag", "Steam", "Discord"},
		},
		{
			name:    "one denied",
			sources: []programSource{source("HKLM 64", []string{"Steam"}, nil), source("HKLM 32", nil, denied), source("HKCU", []string{"ExitLag"}, nil)},
			want:    []string{"Steam", "ExitLag"},
			failed:  []string{"HKLM 32"},
		},
		{
			name:    "one partly read",
			sources: []programSource{source("HKLM 64", []string{"ExitLag"}, cut), source("HKCU", []string{"Discord", ""}, nil)},
			want:    []string{"ExitLag", "Discord"},
			failed:  []string{"HKLM 64"},
		},
		{
			name:    "all denied",
			sources: []programSource{source("HKLM 64", nil, denied), source("HKCU", nil, denied)},
			failed:  []string{"HKLM 64", "HKCU"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := readPrograms(c.sources)
			if !slices.Equal(got, c.want) {
				t.Errorf("names = %q, want %q", got, c.want)
			}
			if (err != nil) != (len(c.failed) > 0) {
				t.Fatalf("err = %v, want failures in %q", err, c.failed)
			}
			for _, f := range c.failed {
				if !strings.Contains(err.Error(), f+": ") {
					t.Errorf("err %q doesn't name %q", err, f)
				}
			}
		})
	}
}

// A partly readable installed-programs list is still matched, and the
// report names the part it couldn't read instead of claiming nothing was
// found there.
func TestScanMatchesPartialPrograms(t *testing.T) {
	_, err := readPrograms([]programSource{
		source(`HKLM\Uninstall (64-bit view)`, []string{"ExitLag"}, nil),
		source(`HKLM\Uninstall (32-bit view)`, nil, errors.New("Access is denied.")),
	})
	sys := static{inv: Inventory{Programs: []string{"ExitLag"}}, errs: map[string]error{"programs": err}}
	r := detector(sys).Scan(context.Background())
	if _, ok := find(r, "exitlag"); !ok {
		t.Errorf("ExitLag from the readable key lost: %v", ids(r))
	}
	if len(r.Unchecked) != 1 || !strings.HasPrefix(r.Unchecked[0], "installed programs: ") || !strings.Contains(r.Unchecked[0], "(32-bit view): Access is denied") {
		t.Errorf("Unchecked = %q, want the 32-bit key named", r.Unchecked)
	}
}
