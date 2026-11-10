package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// SupervisorRuntimeSnapshot is a read-only view of scheduler ownership/capacity.
type SupervisorRuntimeSnapshot struct {
	ActiveBooks        map[int64]bool
	AgentActive        int
	AgentCapacity      int
	EligibleAgentBooks int
	EligibleAgentIDs   []int64
	AgentInvocations   int
	InvocationCapacity int
	InvocationsByBook  map[int64]int
	MaxAgentsPerBook   int
}

func (s *Scheduler) SupervisorRuntime(books []store.Book) SupervisorRuntimeSnapshot {
	out := SupervisorRuntimeSnapshot{ActiveBooks: map[int64]bool{}, AgentCapacity: s.agentCapacity(), InvocationsByBook: map[int64]int{}}
	if runtime, ok := s.exec.(agentInvocationRuntime); ok {
		out.AgentInvocations, out.InvocationsByBook, out.InvocationCapacity = runtime.AgentInvocationRuntime()
	}
	if runtime, ok := s.exec.(agentFanoutRuntime); ok {
		out.MaxAgentsPerBook = runtime.AgentMaxPerBook()
	}
	s.mu.Lock()
	for id, ib := range s.inflight {
		out.ActiveBooks[id] = true
		if ib.lane == state.LaneAgent {
			out.AgentActive++
		}
	}
	s.mu.Unlock()
	holders := lockHolders(books)
	for _, b := range books {
		if out.ActiveBooks[b.ID] {
			continue
		}
		st := state.State(b.State)
		if state.LaneOf(st) == state.LaneAgent && state.CanStart(st, state.Status(b.Status), holders[b.ID]) {
			out.EligibleAgentBooks++
			out.EligibleAgentIDs = append(out.EligibleAgentIDs, b.ID)
		}
	}
	return out
}

// SupervisorApply executes the supervisor's small predefined playbook. The string
// action is mapped at the server seam so scheduler does not depend on supervisor.
func (s *Scheduler) SupervisorApply(ctx context.Context, action string, bookID int64, stage string, detail ...string) (string, error) {
	var b store.Book
	switch action {
	case "retry", "readmit", "terminate_requeue", "supersede_rerun", "stop_budget", "park_escalate":
		var err error
		if b, err = s.db.GetBook(ctx, bookID); err != nil {
			return "", err
		}
		if IsCancelled(b) {
			// A cancel ends the book for good (its job is gone or going): only a
			// person's Retry, or a new job continuing it, takes it up again, and no
			// supervisor action writes over it (bridge#37).
			return "book was cancelled; left as it is", nil
		}
	}
	switch action {
	case "retry", "readmit":
		if b.Status == string(state.StatusFailed) || b.Status == string(state.StatusNeedsAttention) {
			if action == "readmit" && b.RetryAt != "" {
				due, parseErr := time.Parse(time.RFC3339Nano, b.RetryAt)
				if parseErr == nil && time.Now().Before(due) {
					return "existing transient readmission window retained until " + b.RetryAt, nil
				}
			}
			if err := s.readmit(ctx, b); errors.Is(err, store.ErrStatusChanged) {
				return statusChangedOutcome, nil
			} else if err != nil {
				return "", err
			}
			return "book readmitted", nil
		}
		return "book was already admitted", nil
	case "requeue":
		s.notify()
		return "scheduler dispatch nudged", nil
	case "terminate_requeue":
		return s.supervisorTerminateRequeue(ctx, b)
	case "supersede_rerun":
		return s.supervisorSupersedeRerun(ctx, b, stage)
	case "stop_budget":
		return s.supervisorPark(ctx, b, state.ParkSupervisorBudget, "supervisor stopped the stage at its configured duration/token/cost limit")
	case "park_escalate":
		message := "supervisor parked this book for operator review"
		if len(detail) > 0 && detail[0] != "" {
			message = state.SupervisorMessagePrefix + detail[0]
		}
		return s.supervisorPark(ctx, b, state.ParkSupervisorEscalated, message)
	case "reallocate":
		s.notify()
		return "idle configured capacity was offered queued work", nil
	default:
		return "", fmt.Errorf("unsupported supervisor action %q", action)
	}
}

// statusChangedOutcome is a supervisor action's outcome when the book's status
// changed between its read and the action's write (a Cancel landing there,
// bridge#37): the write is never made over it.
const statusChangedOutcome = "the book's status changed meanwhile; left as it is"

// The supervisor's writes below take the book SupervisorApply read (and checked
// isn't cancelled), and each status write holds only while the row still has the
// status and error of that read (store.SetBookStatusIf).

func (s *Scheduler) supervisorTerminateRequeue(ctx context.Context, b store.Book) (string, error) {
	bookID := b.ID
	s.mu.Lock()
	_, active := s.inflight[bookID]
	s.mu.Unlock()
	if active {
		s.cancelInflight(bookID)
		s.notify()
		return "stuck worker terminated; current stage will requeue", nil
	}
	runs, err := s.db.OpenStageRuns(ctx)
	if err != nil {
		return "", err
	}
	closed := false
	for _, r := range runs {
		if r.BookID == bookID {
			if err := s.db.FinishStageRun(ctx, r.ID, false, json.RawMessage(`{"interrupted":true,"supervisor":true}`)); err != nil {
				return "", err
			}
			closed = true
		}
	}
	if b.Status != "" {
		if err := s.db.ClearBookStatusIf(ctx, b.ID, b.Status, b.Error); errors.Is(err, store.ErrStatusChanged) {
			return statusChangedOutcome, nil
		} else if err != nil {
			return "", err
		}
	}
	s.notify()
	if closed {
		return "orphaned database run closed and requeued", nil
	}
	return "no active invocation remained; scheduler nudged", nil
}

func (s *Scheduler) supervisorSupersedeRerun(ctx context.Context, b store.Book, stageName string) (string, error) {
	bookID := b.ID
	stage := state.State(stageName)
	if !state.IsStage(stage) || stage == state.Contributing {
		return "", ErrInvalidOp
	}
	if state.Order(state.State(b.State)) >= state.Order(state.Ready) {
		return "", ErrInvalidOp
	}
	runs, err := s.db.ListStageRuns(ctx, bookID)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		if run.Stage == string(state.Contributing) && run.Ok != nil && *run.Ok && !run.Superseded {
			return "", ErrInvalidOp
		}
	}
	s.cancelInflight(bookID)
	for _, candidate := range state.All() {
		if state.IsStage(candidate) && state.Order(candidate) >= state.Order(stage) {
			if err := s.db.SupersedeStageSuccesses(ctx, bookID, string(candidate)); err != nil {
				return "", err
			}
			_ = os.Remove(SentinelPath(b.WorkDir, string(candidate)))
		}
	}
	// The state alone first (a status set since is kept), then a clear of the
	// status read, unless it is a deliberate pause.
	if err := s.db.SetBookPipelineState(ctx, bookID, string(stage)); err != nil {
		return "", err
	}
	status, errMessage, parkCode := "", "", ""
	if b.Status == string(state.StatusPaused) {
		status, errMessage, parkCode = b.Status, b.Error, b.ParkCode
	} else if err := s.db.ClearBookStatusIf(ctx, bookID, b.Status, b.Error); errors.Is(err, store.ErrStatusChanged) {
		s.notify()
		return statusChangedOutcome, nil
	} else if err != nil {
		return "", err
	}
	s.publishState(bookID, string(stage), status, errMessage, parkCode, "")
	s.notify()
	return "invalidated stage and later sentinels; rerun queued with released inputs/code", nil
}

func (s *Scheduler) supervisorPark(ctx context.Context, b store.Book, code state.ParkCode, reason string) (string, error) {
	bookID := b.ID
	if state.IsTerminal(state.State(b.State)) {
		return "", ErrInvalidOp
	}
	// If the book is ALREADY parked with a typed code, PRESERVE that code (and its
	// scheduled retry_at): the escalation updates only the operator-facing message, never
	// the machine-readable park class. Rewriting a wrapped park to supervisor_escalated (or
	// supervisor_budget) would strip the underlying code's readmit semantics - a Retry on a
	// real fix_loop_exhausted park would take the plain readmit branch (no auditing/fixing
	// supersede, no audit_rounds/audit_accepted wipe), re-enter the audit stage with an
	// exhausted round history, and re-park instantly. Both readmit and supervisor.Decide
	// already switch on the underlying code, so preserving it routes them to the right
	// branch (supervisor_escalated's ask-model/retry branch simply stops being reachable for
	// a wrapped park - that is the intent). The supervisor code is applied only when parking
	// a book that was not already parked.
	effectiveCode, retryAt := code, ""
	if b.Status == string(state.StatusNeedsAttention) && b.ParkCode != "" {
		effectiveCode, retryAt = state.ParkCode(b.ParkCode), b.RetryAt
		// The park's own message is the one for people (the console and the
		// Bridge's page show it beside the park's hint); the supervisor's
		// diagnosis goes to the log, never over it (bridge#24). A park with no
		// message of its own takes the supervisor's.
		if strings.TrimSpace(b.Error) != "" {
			slog.Info("supervisor: a parked book escalated; its park message kept", "book_id", bookID,
				"park_code", b.ParkCode, "diagnosis", reason)
			reason = b.Error
		}
	}
	err := s.db.SetBookStatusIf(ctx, bookID, b.Status, b.Error, string(state.StatusNeedsAttention), reason, string(effectiveCode), retryAt)
	if errors.Is(err, store.ErrStatusChanged) {
		return statusChangedOutcome, nil
	} else if err != nil {
		return "", err
	}
	s.cancelInflight(bookID)
	s.publishState(bookID, b.State, string(state.StatusNeedsAttention), reason, string(effectiveCode), retryAt)
	return "book parked and escalated", nil
}
