package diag

func newPlatformWLAN() WLAN { return newProcWLAN("/", nil) }
