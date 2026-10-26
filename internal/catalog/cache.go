package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Cache keeps the last good copy of each downloaded list, so the game page
// still shows servers when Steam can't be reached.
type Cache interface {
	// Load returns what was saved under key and when; an error (including
	// fs.ErrNotExist) means nothing usable.
	Load(key string) ([]byte, time.Time, error)
	Save(key string, b []byte) error
}

var cacheKeyRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

var errBadKey = errors.New("catalog cache: bad key")

// DirCache is a Cache of one file per key in a folder.
type DirCache struct{ dir string }

func NewDirCache(dir string) *DirCache { return &DirCache{dir: dir} }

func (c *DirCache) path(key string) (string, error) {
	if !cacheKeyRe.MatchString(key) {
		return "", errBadKey
	}
	return filepath.Join(c.dir, key+".json"), nil
}

func (c *DirCache) Load(key string) ([]byte, time.Time, error) {
	p, err := c.path(key)
	if err != nil {
		return nil, time.Time{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, time.Time{}, err
	}
	if fi.Size() > maxConfigBytes {
		return nil, time.Time{}, errors.New("catalog cache: file too large")
	}
	b, err := os.ReadFile(p)
	return b, fi.ModTime(), err
}

// Save writes atomically (temp file, then rename), so a crash mid-write
// leaves the old copy.
func (c *DirCache) Save(key string, b []byte) error {
	p, err := c.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
