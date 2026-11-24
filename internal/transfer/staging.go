package transfer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StagingMaxAge is the age at which abandoned download files can be swept.
const StagingMaxAge = 7 * 24 * time.Hour

// PrivateDir holds a job's resumable files in the Bridge's data directory.
type PrivateDir string

// Create makes the private staging directory and any missing parents.
func (d PrivateDir) Create() error {
	return os.MkdirAll(string(d), 0o700)
}

// Base names a download by account and ASIN or by library and project ID.
// Callers supply validated identifiers and append the suffix for each file.
func (d PrivateDir) Base(owner, item string) string {
	return filepath.Join(string(d), owner+"-"+item)
}

// Sweep visits the private directory in listing order. When files refer to
// companions, inspect reads all regular files before the first removal. The
// job decides which entries it owns and how to remove them.
func (d PrivateDir) Sweep(inspect func(string, fs.DirEntry), sweep func(string, fs.DirEntry) error) error {
	entries, err := os.ReadDir(string(d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if inspect != nil {
		for _, e := range entries {
			if e.Type().IsRegular() {
				inspect(filepath.Join(string(d), e.Name()), e)
			}
		}
	}
	var errs []error
	for _, e := range entries {
		if err := sweep(filepath.Join(string(d), e.Name()), e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
