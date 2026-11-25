package generate

import (
	"context"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/eta"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// AgentState is what the estimates say about the agent container.
type AgentState struct {
	Backend      string
	Ready        bool
	Subscription bool
}

// Estimates computes the heartbeat's sidecars estimates (contract section 2)
// from the store, including the finished books' agent spend
// per hour of audio and per ebook, and this month's spend against the budget.
func Estimates(ctx context.Context, db *store.DB, agent AgentState, monthlyBudgetUSD float64, now time.Time) (*link.SidecarEstimates, error) {
	books, err := db.FinishedBooks(ctx)
	if err != nil {
		return nil, err
	}
	start, _ := pipeline.MonthBounds(now)
	spent, err := db.SumBudgetCostSince(ctx, start)
	if err != nil {
		return nil, err
	}
	e := estimateFrom(books)
	e.AgentBackend, e.AgentReady, e.Subscription = agent.Backend, agent.Ready, agent.Subscription
	e.MonthlyBudgetUSD, e.MonthSpentUSD = monthlyBudgetUSD, Cents(spent)
	return e, nil
}

// estimateFrom summarizes spend and supplies the seed ASR estimate. The heartbeat
// fills in the active route's live rate on every beat. Spend per hour sums the
// audio books (a long book weighs more), per ebook it averages the ebooks; nil
// until a finished book of that kind gives one.
func estimateFrom(books []store.FinishedBook) *link.SidecarEstimates {
	e := &link.SidecarEstimates{ASRSecondsPerAudioHour: eta.SeedASRSecondsPerAudioHour}
	var (
		audioHours, audioCost float64
		ebooks, audioBooks    int
		ebookCost             float64
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
		}
	}
	e.Samples = ebooks + audioBooks
	if audioHours > 0 {
		v := Cents(audioCost / audioHours)
		e.AgentUSDPerAudioHour = &v
	}
	if ebooks > 0 {
		v := Cents(ebookCost / float64(ebooks))
		e.AgentUSDPerEbook = &v
	}
	return e
}
