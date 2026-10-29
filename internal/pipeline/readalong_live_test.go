//go:build asrlive

// The read-along stage on a real book's kept work dir, with real speech
// recognition for the heal. Run with:
//
//	AUDIOSILO_READALONG_WORK=<a COPY of a read-along book's work dir> \
//	  go test -tags asrlive -run TestLiveReadalongAlign -v ./internal/pipeline/
//
// It aligns again from the kept transcripts (as a re-align does), heals with
// mlx-whisper (its venv built under a temp data dir; the Hugging Face cache is
// shared) and logs the coverage. The work dir is written to (the heal, the map, the
// report, the chapter FLACs a purge took), so never point it at a live Bridge's.
package pipeline

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

func TestLiveReadalongAlign(t *testing.T) {
	work := os.Getenv("AUDIOSILO_READALONG_WORK")
	if work == "" {
		t.Skip("AUDIOSILO_READALONG_WORK not set")
	}
	dataDir := os.Getenv("AUDIOSILO_READALONG_DATA")
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	b, capab, err := asr.Select(context.Background(), asr.SelectConfig{Backend: asr.IDMLXWhisper, DataDir: dataDir, Log: log})
	if err != nil || !capab.Available {
		t.Skipf("mlx-whisper not available: %v %s", err, capab.Detail)
	}
	exe := NewExecutor(Config{DataDir: dataDir, Log: log, Fallback: scheduler.NewStubExecutor(0, 0),
		ASR: ASRSetup{Backend: b, Cap: capab, Model: asr.DefaultModelFor(capab.Backend), Language: "en"}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	start := time.Now()
	if _, err := exe.Execute(ctx, store.Book{ID: 1, WorkDir: work, Goal: state.GoalReadalong}, state.ReadalongAligning, scheduler.StageReport{}); err != nil {
		t.Fatalf("readalong_aligning: %v", err)
	}
	rep, err := ReadReadalongReport(work)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("in %s: text %.4f (first pass %.4f), audio %.4f, %d/%d sentences, %d heal windows (%d loops, %.1f s), %d audio gaps",
		time.Since(start).Round(time.Second), rep.Coverage.Text, rep.FirstPassCoverage, rep.Coverage.Audio,
		rep.AlignedSentences, rep.Sentences, rep.HealWindows, rep.HealLoopWindows, rep.HealSeconds, rep.AudioGaps)
}
