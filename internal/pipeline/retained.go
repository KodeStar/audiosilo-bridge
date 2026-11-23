package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// manifestAtBook resolves source reads against the engine book. The stored
// manifest stays exactly as ASR fingerprinted it, including across a move.
func manifestAtBook(m audio.Manifest, source string) (audio.Manifest, error) {
	if source == "" || source == m.Source || (m.Style != audio.StyleFiles && source == filepath.Dir(m.Source)) {
		return m, nil
	}
	src, err := audio.ResolveSourceWithin(source, SourceStatWait)
	if err != nil {
		return m, err
	}
	if m.Style != audio.StyleFiles && src.Style == audio.StyleMarkers {
		m.Source = src.BookFile
		return m, nil
	}
	if m.Style != audio.StyleFiles || src.Style != audio.StyleFiles {
		return m, errors.New("the moved book has a different audio layout")
	}
	files := make(map[string]string, len(src.Files))
	for _, f := range src.Files {
		files[filepath.Base(f)] = f
	}
	m.Chapters = slices.Clone(m.Chapters)
	for i, c := range m.Chapters {
		f, ok := files[filepath.Base(c.FilePath)]
		if !ok {
			return m, fmt.Errorf("the moved book has no audio file %q", filepath.Base(c.FilePath))
		}
		m.Chapters[i].FilePath = f
	}
	m.Source = source
	return m, nil
}

// KeptTranscription checks every raw chapter against the manifest and its
// recorded ASR fingerprint. Older runs can predate the fingerprint.
func KeptTranscription(ctx context.Context, dir string, m audio.Manifest) (bool, error) {
	if len(m.Chapters) == 0 {
		return false, nil
	}
	var provenance asrProvenance
	raw, err := os.ReadFile(filepath.Join(dir, asrInfoName)) //nolint:gosec // the retained run's provenance
	switch {
	case err == nil:
		if json.Unmarshal(raw, &provenance) != nil {
			return false, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	if sum := provenance.ManifestSHA; sum != "" {
		fp, err := manifestFingerprint(dir)
		if err != nil || fp != sum {
			return false, err
		}
	}
	for _, c := range m.Chapters {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !rawComplete(filepath.Join(dir, transcript.RawDir, transcript.RawName(c.Chapter))) {
			return false, nil
		}
	}
	return true, nil
}

// SeedTranscription installs just the immutable inputs a fresh run needs
// after ASR. Agent outputs, filing history and stage sentinels start fresh.
func SeedTranscription(ctx context.Context, src, dst string, m audio.Manifest) error {
	if err := os.MkdirAll(filepath.Join(dst, transcript.RawDir), 0o750); err != nil {
		return err
	}
	names := []string{audio.ManifestName, audio.ProbeName}
	if fsutil.IsFile(filepath.Join(src, asrInfoName)) {
		names = append(names, asrInfoName)
	}
	if m.Detected {
		names = append(names, detectedChaptersName, detectedPositionsName)
	}
	for _, c := range m.Chapters {
		names = append(names, filepath.Join(transcript.RawDir, transcript.RawName(c.Chapter)))
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		if err := os.Link(from, to); err == nil {
			continue
		} else if !fsutil.LinkUnsupported(err) {
			return err
		}
		info, err := os.Stat(from)
		if err != nil {
			return err
		}
		if err := fsutil.CopyFile(from, to, info.Mode().Perm()); err != nil {
			return err
		}
		// In particular, probe's original time bounds the audio freshness check.
		if err := os.Chtimes(to, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
	}
	return nil
}
