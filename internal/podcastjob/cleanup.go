package podcastjob

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// cleanOwned considers only named checkpoint records and committed trash
// manifests older than a day. Unrecorded hidden files are never swept.
func (s *service) cleanOwned(ctx context.Context, root *os.Root, lib link.Library, mount, show string) {
	if s.o.DB == nil || s.o.DataDir == "" || s.writable(lib, mount) != nil {
		return
	}
	rows, err := s.o.DB.OpenServerJobs(ctx)
	if err != nil {
		return
	}
	live := func(p string) bool {
		for _, r := range rows {
			if r.ServerID == s.o.ServerID() && r.LibraryID != nil && *r.LibraryID == lib.ID && r.Path != nil && (*r.Path == p || *r.Path == show) && r.State == store.JobRunning {
				return true
			}
		}
		return false
	}
	old := func(stamp string) bool {
		t, err := time.Parse(time.RFC3339, stamp)
		return err == nil && s.o.Now().Sub(t) > 24*time.Hour
	}
	locked := func(p string, fn func()) {
		wait, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		release, err := s.fileLock(wait, root, p)
		if err != nil {
			return
		}
		defer release()
		if s.writable(lib, mount) == nil {
			fn()
		}
	}
	if checkpoints, err := s.checkpointRoot(); err == nil {
		defer func() { _ = checkpoints.Close() }()
		if entries, err := readDir(checkpoints); err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				rd, err := podcast.OpenRegular(checkpoints, entry.Name())
				if err != nil {
					continue
				}
				var c checkpoint
				err = json.NewDecoder(io.LimitReader(rd, 16384)).Decode(&c)
				_ = rd.Close()
				if err != nil || c.ServerHash != s.pairHash() || c.LibraryID != lib.ID || !podcast.ValidEpisodePath(c.Path) || path.Dir(c.Path) != show || !old(c.UpdatedAt) || live(c.Path) || !podcast.ValidID(c.ShowID) || !podcast.ValidID(c.EpisodeID) || !ownedTemp(c.Temp) {
					continue
				}
				job := jobs.Job{LibraryID: &lib.ID, Path: &c.Path}
				if entry.Name() != s.checkpointName(job) {
					continue
				}
				if _, _, err := s.readEpisode(root, c.Path, fetchParams{c.ShowID, c.EpisodeID}); err != nil {
					continue
				}
				locked(c.Path, func() { s.clearCheckpoint(job, root, &c) })
			}
		}
	}
	dir := show + "/.audiosilo-trash/podcasts"
	st, err := root.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return
	}
	// OpenRegular below checks every ancestor before a manifest can authorize IO.
	d, err := root.Open(dir)
	if err != nil {
		return
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rd, err := podcast.OpenRegular(root, dir+"/"+entry.Name()+"/transaction.json")
		if err != nil {
			continue
		}
		var j podcast.Journal
		err = json.NewDecoder(io.LimitReader(rd, 4<<20)).Decode(&j)
		_ = rd.Close()
		if err != nil || !podcast.ValidEpisodePath(j.Path) || path.Dir(j.Path) != show || j.TrashDir != ".audiosilo-trash/podcasts/"+entry.Name() || !old(j.Fetch.DeletedAt) || live(j.Path) {
			continue
		}
		locked(j.Path, func() { _ = podcast.CleanupTrash(root, j) })
	}
}
