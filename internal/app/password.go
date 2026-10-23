package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kodestar/audiosilo-bridge/internal/auth"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// ResetPassword gives the Bridge's page a new generated password and signs out
// every session, for an owner who lost the one the first start printed. It opens
// only the database in dataDir and changes only the password hash and the
// sessions: the pairing, the library mappings, the jobs and the sealed secrets
// stay as they were. A running Bridge reads both from the database, so it takes
// effect at once. It refuses a folder with no Bridge database (a mistyped --data
// would otherwise start an empty one), and a root caller in a folder another
// user owns (the files SQLite adds would be root's, and the Bridge couldn't open
// its database at the next start).
func ResetPassword(ctx context.Context, dataDir string) (string, error) {
	dbPath := filepath.Join(dataDir, store.FileName)
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no Bridge data in %s: pass --data (or set AUDIOSILO_BRIDGE_DATA) to the Bridge's data folder", dataDir)
		}
		return "", err
	}
	if err := checkOwner(dataDir); err != nil {
		return "", err
	}
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return "", err
	}
	pw, err := auth.New(db.AuthStore()).ResetPassword()
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return pw, nil
}

// ownerRefusal is checkOwner's rule, kept pure for its test: root (euid 0) in
// a folder another user owns is told to run as that user.
func ownerRefusal(euid, uid, gid int, dir string) error {
	if euid != 0 || uid == 0 {
		return nil
	}
	return fmt.Errorf("%s belongs to %d:%d; run this as that user so the database stays theirs "+
		"(in Docker: docker exec -u %d:%d <container> audiosilo-bridge reset-password)", dir, uid, gid, uid, gid)
}
