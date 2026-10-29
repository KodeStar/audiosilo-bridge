package repair

// Full-chapter adoption plausibility (EXTRACTION-AUDIO.md step 3.5). Unlike the
// tail-clip machinery above, this has NO verbatim Python source: it encodes the
// judgment the human made by eye when deciding whether a FRESH full-chapter
// re-transcription (of a wph-outlier or mid-chapter-loop chapter) should replace the
// original. The load-bearing invariant it enforces is "NEVER blindly adopt the fresh
// run" - one validated chapter kept its original after two re-runs collapsed at the
// same point. So the numbers here are an adapted heuristic, not a contract, and are
// documented as such.

// Narration physics used to judge plausibility (~140-150 words/minute of real
// audiobook narration, EXTRACTION-AUDIO.md).
const (
	normWPMLow  = 140.0
	normWPMHigh = 150.0
	// A transcript is implausible-LOW below half the slow end (a collapsed/truncated
	// run that dropped most of the chapter) and implausible-HIGH above twice the fast
	// end (a degenerate loop cramming words into the runtime).
	plausibleMinWPM = normWPMLow * 0.5
	plausibleMaxWPM = normWPMHigh * 2
)

// AdoptStats is the minimal per-transcript signal AdoptFresh weighs: the chapter's
// word count and its audio duration.
type AdoptStats struct {
	Words       int
	DurationSec float64
	// LoopWords are the words inside the transcript's 6-gram loops (qa.LoopWords);
	// only the read-along's rule weighs them.
	LoopWords int
}

// AdoptDecision is AdoptFresh's verdict.
type AdoptDecision struct {
	Adopt  bool
	Reason string
}

// wpm returns words per minute, or 0 for a non-positive duration.
func (s AdoptStats) wpm() float64 {
	if s.DurationSec <= 0 {
		return 0
	}
	return float64(s.Words) / (s.DurationSec / 60)
}

// plausible reports whether the transcript's word rate is within the sane narration
// band. A rate far above the band is a crammed loop; far below is a collapse.
func (s AdoptStats) plausible() bool {
	w := s.wpm()
	return w >= plausibleMinWPM && w <= plausibleMaxWPM
}

// AdoptFresh decides whether to adopt a freshly re-transcribed full chapter over the
// original, comparing word count and duration plausibility. It NEVER blindly adopts:
//
//   - fresh implausible (collapsed or re-degenerated)  -> keep original.
//   - fresh plausible, original implausible            -> adopt (the re-run fixed it).
//   - both plausible, fresh recovered MORE words       -> adopt (dropped narration
//     recovered, e.g. RLF03 ch056 +417w).
//   - both plausible, fresh not longer                 -> keep original (no gain;
//     prefer the stable original).
//
// It is a pure function (no I/O) and table-tested.
func AdoptFresh(original, fresh AdoptStats) AdoptDecision {
	if !fresh.plausible() {
		return AdoptDecision{Adopt: false, Reason: "fresh run implausible by word rate (collapsed or re-degenerated); keep original"}
	}
	if !original.plausible() {
		return AdoptDecision{Adopt: true, Reason: "fresh run plausible where the original was not; adopt"}
	}
	if fresh.Words > original.Words {
		return AdoptDecision{Adopt: true, Reason: "both plausible, fresh recovered more words; adopt"}
	}
	return AdoptDecision{Adopt: false, Reason: "both plausible, fresh not longer; keep original"}
}

// readalongMinKept is the share of the original's words outside its loops a fresh
// read-along re-run must keep: below it the fresh run dropped narration (a collapse).
const readalongMinKept = 0.8

// AdoptFreshReadalong is a read-along book's adoption rule (Phase 3c-1): the
// fresh NoContext re-run of a chapter the transcript check flagged replaces the
// original unless it is clearly worse. A loop's words are noise, not narration,
// so word count alone can't judge a looped original (it has MORE words; AdoptFresh
// keeps it), and the spike adopted every re-run:
//
//   - fresh implausible by word rate (collapsed or crammed: AdoptFresh's check)
//     -> keep original (the heal takes that span).
//   - fresh loops over more words than the original -> keep original.
//   - original loop-free (a re-run for dropout, a low word rate or a fast tail):
//     fresh shorter -> keep original; else adopt.
//   - fresh keeps under 80% of the original's words outside its loops (a collapse
//     that dropped narration)                       -> keep original.
//   - otherwise                                      -> adopt.
//
// generate's whole run keeps AdoptFresh. Pure and table-tested.
func AdoptFreshReadalong(original, fresh AdoptStats) AdoptDecision {
	switch {
	case !fresh.plausible():
		return AdoptDecision{Adopt: false, Reason: "fresh run implausible by word rate (collapsed or re-degenerated); keep original"}
	case fresh.LoopWords > original.LoopWords:
		return AdoptDecision{Adopt: false, Reason: "fresh run loops over more words; keep original"}
	case original.LoopWords == 0 && fresh.Words < original.Words:
		return AdoptDecision{Adopt: false, Reason: "original has no loop and the fresh run is shorter; keep original"}
	case original.LoopWords == 0:
		return AdoptDecision{Adopt: true, Reason: "original has no loop and the fresh run is no shorter; adopt"}
	case float64(fresh.Words-fresh.LoopWords) < readalongMinKept*float64(original.Words-original.LoopWords):
		return AdoptDecision{Adopt: false, Reason: "fresh run dropped narration the original had outside its loops; keep original"}
	}
	return AdoptDecision{Adopt: true, Reason: "fresh run no loopier and keeps the narration; adopt"}
}
