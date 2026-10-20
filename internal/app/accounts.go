package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/accounts"
	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/ghauth"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/setup"
	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// The Accounts tab's backend (Phase 2b): the GitHub sign-in a contribution files
// with, the agent's credential the Bridge hands to the agent container, and the
// setup checklist the heartbeat carries.

// newAccounts builds the GitHub sign-in and the agent's credential store over
// the vault, and the contribution's token source over the sign-in.
func (b *Bridge) newAccounts(cfg config.Config) {
	b.github = accounts.NewGitHub(accounts.GitHubOptions{Vault: b.vault, DB: b.db, Log: b.log,
		Client:   &ghauth.Client{ClientID: cfg.GitHubClientID(), WebBase: cfg.GitHubWebBase(), APIBase: cfg.Contribution.APIBaseURL},
		OnChange: b.githubChanged})
	b.agentCreds = accounts.NewAgent(b.vault, b.db, nil, b.agentCredentialChanged)
	b.tokens = contrib.NewTokenSource(b.github, b.githubChanged)
}

// forgetSealedGitHubPAT deletes the github_pat an earlier version sealed from
// AUDIOSILO_BRIDGE_GITHUB_TOKEN at every start: the token source reads the
// environment directly now, so a sealed copy would outlive its variable. Best
// effort: nothing reads that copy any more, so one that can't be opened (sealed
// with another secret key) or deleted never stops the start.
func forgetSealedGitHubPAT(vault secrets.Store, log *slog.Logger) {
	if ok, err := vault.Present(secrets.GitHubPAT); err == nil && !ok {
		return
	}
	if err := vault.Delete(secrets.GitHubPAT); err != nil {
		log.Warn("bridge: removing the sealed copy of "+contrib.GitHubTokenEnv+" failed (nothing reads it)", "err", err)
		return
	}
	log.Info("bridge: removed the sealed copy of " + contrib.GitHubTokenEnv +
		" (the environment is read directly now; sign in on the Accounts tab instead)")
}

// githubChanged runs after a GitHub sign-in, sign-out or refusal: the heartbeat
// says so at once, and the books waiting for a credential try again.
func (b *Bridge) githubChanged() {
	b.Kick()
	go b.readmitContributions(context.Background())
}

// readmitContributions re-admits the books parked waiting for a GitHub credential
// once one resolves, and reports how many.
func (b *Bridge) readmitContributions(ctx context.Context) int {
	if e := b.engine.get(); e != nil && b.canContribute(ctx) {
		return e.sched.ReadmitParked(ctx, "a GitHub credential is available again", state.ParkContribUnavailable)
	}
	return 0
}

// agentCredentialChanged runs after the Accounts tab changed the agent's
// credential: the books and describe jobs parked on a refused or missing one
// try again, and the heartbeat reports the agent's state at once.
func (b *Bridge) agentCredentialChanged() {
	b.Kick()
	codes := []state.ParkCode{state.ParkAgentCredentialRefused, state.ParkAgentUnavailable}
	for _, code := range codes {
		b.wakeParked(code) // a describe job waits in its own loop
	}
	go func() {
		ctx := context.Background()
		if e := b.engine.get(); e != nil {
			for _, code := range codes {
				e.sched.ReadmitParked(ctx, "the agent's credential changed on the Accounts tab", code)
			}
		}
	}()
}

// agentCredential is the agent box's hand-off source.
func (b *Bridge) agentCredential(cli string) (kind, value string, since time.Time, ok bool) {
	return b.agentCreds.Credential(context.Background(), cli)
}

// canContribute reports whether a GitHub token resolves, which a job with
// contribute=issue needs.
func (b *Bridge) canContribute(ctx context.Context) bool { return b.contributeFrom(ctx) != "" }

// contribCredential is why no contribution token resolves now (nil when one does).
func (b *Bridge) contribCredential(ctx context.Context) error {
	_, _, err := b.tokens.Resolve(ctx)
	return err
}

// contributeFrom is where a contribution's token comes from now ("" for none).
func (b *Bridge) contributeFrom(ctx context.Context) string {
	_, from, err := b.tokens.Resolve(ctx)
	if err != nil {
		return ""
	}
	return from
}

// Accounts is the Accounts tab's state.
func (b *Bridge) Accounts(ctx context.Context) api.Accounts {
	gs := b.github.State(ctx)
	from := b.contributeFrom(ctx)
	gh := api.GitHubAccount{Available: gs.Available, SignedIn: gs.SignedIn, Refused: gs.Refused, Login: gs.Login,
		SignedInAt: gs.SignedInAt, LastOKAt: gs.LastOKAt, LastError: gs.LastError, FlowError: gs.FlowError,
		EnvToken: strings.TrimSpace(os.Getenv(contrib.GitHubTokenEnv)) != "", From: from, CanContribute: from != "",
		ManageURL: ghauth.AuthorizationsURL}
	if p := gs.Pending; p != nil {
		gh.Pending = &api.GitHubPending{UserCode: p.UserCode, VerificationURI: p.VerificationURI, ExpiresAt: p.ExpiresAt}
	}
	as := b.agentCreds.State(ctx)
	st := b.box.Status()
	backend := b.box.ID()
	ag := api.AgentAccount{Backend: backend, Kinds: accounts.KindsFor(backend), Set: as.Set, SetAt: as.SetAt,
		SetAtByKind: as.SetAtByKind,
		Available:   st.Available, Detail: st.Detail, Handoff: st.Handoff}
	if !st.Handoff {
		ag.ContainerCredential = st.CLIs[backend].Credential
	}
	return api.Accounts{GitHub: gh, Agent: ag}
}

// StartGitHubSignIn begins the device flow.
func (b *Bridge) StartGitHubSignIn(ctx context.Context) (api.GitHubPending, error) {
	p, err := b.github.Start(ctx)
	var gerr *ghauth.Error
	switch {
	case errors.Is(err, ghauth.ErrNoClientID):
		return api.GitHubPending{}, api.Errorf(api.ErrRefused, "%v", err)
	case errors.As(err, &gerr):
		return api.GitHubPending{}, api.Errorf(api.ErrRefused, "GitHub refused the sign-in: %v", err)
	case err != nil && ctx.Err() == nil:
		return api.GitHubPending{}, api.Errorf(api.ErrUnreachable, "could not reach GitHub: %v", err)
	case err != nil:
		return api.GitHubPending{}, err
	}
	return api.GitHubPending{UserCode: p.UserCode, VerificationURI: p.VerificationURI, ExpiresAt: p.ExpiresAt}, nil
}

// SignOutGitHub forgets the sign-in and revokes it on GitHub (best effort).
func (b *Bridge) SignOutGitHub(ctx context.Context) (string, error) {
	revokeErr, err := b.github.SignOut(ctx)
	if err != nil {
		return "", err
	}
	if revokeErr != nil {
		return "the sign-in is gone from this Bridge, but GitHub didn't confirm the revoke: review the App's access on GitHub", nil
	}
	return "", nil
}

// SetAgentCredential seals one agent credential.
func (b *Bridge) SetAgentCredential(ctx context.Context, kind, value string) error {
	err := b.agentCreds.Set(ctx, kind, value)
	if errors.Is(err, accounts.ErrInvalidCredential) {
		// The message names the kind and the problem, never the value.
		return api.Errorf(api.ErrInvalid, "%v", err)
	}
	return err
}

// DeleteAgentCredential removes one agent credential kind ("" for all).
func (b *Bridge) DeleteAgentCredential(ctx context.Context, kind string) error {
	err := b.agentCreds.Delete(ctx, kind)
	if errors.Is(err, accounts.ErrInvalidCredential) {
		return api.Errorf(api.ErrInvalid, "%v", err)
	}
	return err
}

// setup is the setup checklist now (FIRST-RUN-PLAN.md F2).
func (b *Bridge) setup(ctx context.Context) link.Setup {
	cfg := b.config()
	in := setup.Inputs{Paired: b.conn.Client() != nil, BudgetReviewed: cfg.BudgetReviewed(),
		MonthlyBudgetUSD: cfg.Agent.MonthlyBudgetUSD}
	for _, s := range b.libs.Statuses() {
		in.Libraries = append(in.Libraries, setup.Library{ID: s.LibraryID, Mapped: s.Mapped, Writable: s.Writable})
	}
	if b.engine.get() != nil {
		in.ASR.Started = true
		if r := b.router.Load(); r != nil {
			connected := r.Hub.Connected()
			localOK := r.Local != nil && r.LocalCap.Available
			in.ASR.Remote = connected
			in.ASR.Waiting = !connected && r.Wait()
			in.ASR.Available = connected || localOK
			in.ASR.Detail = r.LocalCap.Detail
		}
	}
	st := b.box.Status()
	in.Agent = setup.Agent{Backend: b.box.ID(), Available: st.Available, Detail: st.Detail}
	if from := b.contributeFrom(ctx); from != "" {
		in.GitHub = setup.GitHub{CanContribute: true, From: from}
		if from == contrib.FromDevice {
			in.GitHub.Login = b.github.Login(ctx)
		}
	}
	return setup.Checklist(in)
}
