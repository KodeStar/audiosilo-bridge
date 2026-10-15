package generate

import (
	"context"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// SeedASRSecondsPerAudioHour is the ASR estimate before this Bridge has learned
// its own rate: a quarter of the audio's length, a CPU whisper.cpp ballpark.
const SeedASRSecondsPerAudioHour = 900

// AgentState is what the estimates say about the agent container.
type AgentState struct {
	Backend      string
	Ready        bool
	Subscription bool
}

// Estimates computes the heartbeat's sidecars estimates (contract section 2)
// from the store: the ASR stage's learned rate, the finished books' agent spend
// per hour of audio and per ebook, and this month's spend against the budget.
func Estimates(ctx context.Context, db *store.DB, agent AgentState, monthlyBudgetUSD float64, now time.Time) (*link.SidecarEstimates, error) {
	books, err := db.FinishedBooks(ctx)
	if err != nil {
		return nil, err
	}
	rate, hasRate, err := db.GetRate(ctx, string(state.ASR))
	if err != nil {
		return nil, err
	}
	spent, err := db.SumBudgetCostSince(ctx, MonthStart(now))
	if err != nil {
		return nil, err
	}
	e := estimateFrom(books, rate, hasRate)
	e.AgentBackend, e.AgentReady, e.Subscription = agent.Backend, agent.Ready, agent.Subscription
	e.MonthlyBudgetUSD, e.MonthSpentUSD = monthlyBudgetUSD, cents(spent)
	return e, nil
}

// MonthStart is the start of now's calendar month, UTC: the monthly budget's
// month (the pipeline's runAgent counts the same one).
func MonthStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// estimateFrom is the figures' arithmetic. The ASR stage's rate is seconds per
// chapter, so it is scaled by the finished audio books' chapters per hour; with
// no rate or no such book the seed stands. Agent spend per audio hour sums the
// audio books (a long book weighs more), per ebook it averages the ebooks; nil
// until a finished book of that kind gives one.
func estimateFrom(books []store.FinishedBook, asrRate float64, hasRate bool) *link.SidecarEstimates {
	e := &link.SidecarEstimates{ASRSecondsPerAudioHour: SeedASRSecondsPerAudioHour}
	var (
		audioHours, audioCost, chapterHours float64
		chapters, ebooks, audioBooks        int
		ebookCost                           float64
	)
	for _, b := range books {
		switch state.ParseKind(b.Kind) {
		case state.KindEbook:
			ebooks++
			ebookCost += b.CostUSD
		default:
			if b.DurationSec <= 0 {
				continue
			}
			audioBooks++
			audioHours += b.DurationSec / 3600
			audioCost += b.CostUSD
			if b.Chapters > 0 {
				chapters += b.Chapters
				chapterHours += b.DurationSec / 3600
			}
		}
	}
	e.Samples = ebooks + audioBooks
	if audioHours > 0 {
		v := cents(audioCost / audioHours)
		e.AgentUSDPerAudioHour = &v
	}
	if ebooks > 0 {
		v := cents(ebookCost / float64(ebooks))
		e.AgentUSDPerEbook = &v
	}
	if hasRate && asrRate > 0 && chapterHours > 0 {
		e.ASRSecondsPerAudioHour = float64(int64(asrRate * float64(chapters) / chapterHours))
	}
	return e
}
