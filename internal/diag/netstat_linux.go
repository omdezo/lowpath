package diag

import "os"

func newPlatformNetStats() NetStats { return NewSysfsNetStats(os.DirFS("/"), nil) }
