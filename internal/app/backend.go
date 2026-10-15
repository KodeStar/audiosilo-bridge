package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/logring"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The Bridge is the page API's backend.
var _ api.Backend = (*Bridge)(nil)

// Status is the Bridge's state for its page.
func (b *Bridge) Status() api.Status {
	cfg := b.config()
	b.mu.Lock()
	st := api.Status{Version: b.version, Pairing: b.conn.Pairing(), LastError: b.beatErr,
		Kinds: b.runner.Kinds(), Lanes: b.runner.Lanes(), SecretKey: string(b.keySrc), PublicURL: cfg.PublicURL,
		ServerURLDefault: cfg.Server.URL}
	if !b.lastBeat.IsZero() {
		t := b.lastBeat.UTC()
		st.LastHeartbeat = &t
		st.Online = time.Since(b.lastBeat) < 2*time.Minute && b.beatErr == ""
	}
	b.mu.Unlock()
	return st
}

// Pair pairs with the server at serverURL using a code from its console.
func (b *Bridge) Pair(ctx context.Context, serverURL, code string) error {
	err := b.conn.Pair(ctx, serverURL, code)
	switch {
	case errors.Is(err, link.ErrUnauthorized):
		return api.Errorf(api.ErrRefused, "the code is wrong, used or expired; make a new one in the server's console")
	case errors.Is(err, ErrAlreadyPaired):
		return api.Errorf(api.ErrConflict, "%v", err)
	case err != nil:
		var apiErr *link.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			return api.Errorf(api.ErrRefused, "that server doesn't know about a Bridge (update it to a version with the Bridge)")
		}
		return api.Errorf(api.ErrUnreachable, "could not reach the server: %v", err)
	}
	return nil
}

// Unpair forgets the Bridge's token here.
func (b *Bridge) Unpair(ctx context.Context) error {
	return b.conn.Unpair(ctx, "unpaired from the Bridge's page (unpair it in the server's console too)")
}

// Libraries lists the server's libraries with their mappings.
func (b *Bridge) Libraries() []api.Library {
	cfg := b.config()
	locked := cfg.EnvSet(config.KeyLibraries)
	out := []api.Library{}
	for _, e := range b.libs.Entries(b.mount) {
		out = append(out, api.Library{Entry: e, Locked: locked})
	}
	return out
}

// SetMount maps a library to a folder ("" removes the mapping: the server's
// root path is used) and checks the mappings again.
func (b *Bridge) SetMount(ctx context.Context, id int64, path string) error {
	if b.config().EnvSet(config.KeyLibraries) {
		return api.Errorf(api.ErrLocked, "AUDIOSILO_BRIDGE_LIBRARIES sets the mappings")
	}
	if _, ok := b.libs.Library(id); !ok {
		return api.Errorf(api.ErrNotFound, "no server library %d", id)
	}
	if path != "" && !filepath.IsAbs(path) {
		return api.Errorf(api.ErrInvalid, "the folder must be an absolute path")
	}
	err := b.save(func(c *config.Config) error {
		kept := []config.LibraryMapping{}
		for _, m := range c.Libraries {
			if m.LibraryID != id {
				kept = append(kept, m)
			}
		}
		if path != "" {
			kept = append(kept, config.LibraryMapping{LibraryID: id, MountPath: path})
		}
		c.Libraries = kept
		return nil
	})
	if err != nil {
		return err
	}
	b.CheckMappings(context.WithoutCancel(ctx))
	return nil
}

// CheckNow checks every mapping again now.
func (b *Bridge) CheckNow(ctx context.Context) { b.CheckMappings(context.WithoutCancel(ctx)) }

// Settings are the page's editable settings.
func (b *Bridge) Settings() api.Settings {
	cfg := b.config()
	s := api.Settings{PublicURL: cfg.PublicURL, MappingCheckMinutes: cfg.Mapping.CheckMinutes, Locked: []string{}}
	for _, k := range []string{config.KeyPublicURL, config.KeyMappingCheck, config.KeyLibraries, config.KeyServerURL} {
		if cfg.EnvSet(k) {
			s.Locked = append(s.Locked, k)
		}
	}
	return s
}

// SaveSettings saves the page's settings (a locked one must keep its value).
func (b *Bridge) SaveSettings(ctx context.Context, s api.Settings) error {
	cur := b.config()
	if cur.EnvSet(config.KeyPublicURL) && s.PublicURL != cur.PublicURL {
		return api.Errorf(api.ErrLocked, "AUDIOSILO_BRIDGE_PUBLIC_URL sets public_url")
	}
	if cur.EnvSet(config.KeyMappingCheck) && s.MappingCheckMinutes != cur.Mapping.CheckMinutes {
		return api.Errorf(api.ErrLocked, "AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES sets mapping.check_minutes")
	}
	err := b.save(func(c *config.Config) error {
		c.PublicURL, c.Mapping.CheckMinutes = s.PublicURL, s.MappingCheckMinutes
		return nil
	})
	if err != nil {
		return err
	}
	b.Kick()
	return nil
}

// save edits config.yaml and reloads the configuration (environment included).
// An edit that doesn't validate is api.ErrInvalid.
func (b *Bridge) save(edit func(*config.Config) error) error {
	if err := config.SaveFile(b.dataDir, edit); err != nil {
		return api.Errorf(api.ErrInvalid, "%v", err)
	}
	cfg, err := config.Load(b.dataDir)
	if err != nil {
		return err
	}
	cfg.Listen = b.config().Listen // the listener in use; a new listen applies at the next start
	b.cfg.Store(&cfg)
	return nil
}

// Jobs lists the jobs this Bridge claimed, newest first.
func (b *Bridge) Jobs(ctx context.Context, before int64, limit int) ([]store.ServerJob, error) {
	return b.db.ListServerJobs(ctx, before, limit)
}

// CancelJob stops a job running here (reported to the server as cancelled).
func (b *Bridge) CancelJob(id int64) bool { return b.runner.Cancel(id) }

// Logs returns recent log lines.
func (b *Bridge) Logs(after uint64, limit int, level slog.Level) []logring.Entry {
	if b.ring == nil {
		return []logring.Entry{}
	}
	return b.ring.Since(after, limit, level)
}

// Workers lists the ASR workers (none until Phase 1b).
func (b *Bridge) Workers() []link.Worker { return []link.Worker{} }
