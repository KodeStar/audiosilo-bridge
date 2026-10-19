import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AccountsTab } from './AccountsTab';
import { ApiClient } from '@/api/client';
import type { Accounts } from '@/api/types';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { accountsFixture } from '@/test/fixtures';

const api = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

function signedIn(): Accounts {
  const a = accountsFixture();
  return {
    ...a,
    github: {
      ...a.github,
      signed_in: true,
      login: 'octocat',
      signed_in_at: '2026-10-19T10:00:00Z',
      from: 'device',
      can_contribute: true,
    },
  };
}

describe('AccountsTab', () => {
  it('signs in to GitHub with the device code, and goes on once GitHub has it', async () => {
    const pending: Accounts = {
      ...accountsFixture(),
      github: {
        ...accountsFixture().github,
        pending: {
          user_code: 'WDJB-MJHT',
          verification_uri: 'https://github.com/login/device',
          expires_at: new Date(Date.now() + 15 * 60000).toISOString(),
        },
      },
    };
    let state = accountsFixture();
    const { calls } = mockFetch({
      'GET /api/accounts': () => ({ body: state }),
      'POST /api/accounts/github/device': () => {
        state = pending;
        return { body: pending.github.pending };
      },
    });
    render(<AccountsTab client={api()} />);
    const card = await screen.findByRole('region', { name: 'GitHub' });
    expect(within(card).getByText('Not signed in')).toBeInTheDocument();
    await userEvent.click(within(card).getByRole('button', { name: 'Sign in to GitHub' }));
    expect(await within(card).findByLabelText('Sign-in code')).toHaveTextContent('WDJB-MJHT');
    expect(
      within(card).getByRole('link', { name: 'https://github.com/login/device' }),
    ).toHaveAttribute('href', 'https://github.com/login/device');
    expect(within(card).getByText(/15 more minutes/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/accounts/github/device')).toBeDefined();
  });

  it('shows the sign-in, signs out, and passes on a revoke GitHub did not confirm', async () => {
    const { calls } = mockFetch({
      'GET /api/accounts': { body: signedIn() },
      'DELETE /api/accounts/github': {
        body: { accounts: accountsFixture(), revoke_error: "GitHub didn't confirm the revoke" },
      },
    });
    render(<AccountsTab client={api()} />);
    const card = await screen.findByRole('region', { name: 'GitHub' });
    expect(within(card).getByText('octocat')).toBeInTheDocument();
    expect(within(card).getByText('This sign-in')).toBeInTheDocument();
    expect(within(card).getByRole('link', { name: 'Review access on GitHub' })).toHaveAttribute(
      'href',
      'https://github.com/settings/apps/authorizations',
    );
    await userEvent.click(within(card).getByRole('button', { name: 'Sign out' }));
    expect(await within(card).findByText("GitHub didn't confirm the revoke")).toBeInTheDocument();
    expect(within(card).getByText('Not signed in')).toBeInTheDocument();
    expect(lastCall(calls, 'DELETE', '/api/accounts/github')).toBeDefined();
  });

  it('asks to sign in again after GitHub refused the sign-in, and names an env token', async () => {
    const a = signedIn();
    mockFetch({
      'GET /api/accounts': {
        body: {
          ...a,
          github: {
            ...a.github,
            refused: true,
            last_error: 'GitHub refused the sign-in',
            env_token: true,
          },
        },
      },
    });
    render(<AccountsTab client={api()} />);
    const card = await screen.findByRole('region', { name: 'GitHub' });
    expect(within(card).getByText('Sign in again')).toBeInTheDocument();
    expect(within(card).getByText('GitHub refused the sign-in')).toBeInTheDocument();
    expect(within(card).getByText(/AUDIOSILO_BRIDGE_GITHUB_TOKEN is set/)).toBeInTheDocument();
    expect(within(card).getByRole('button', { name: 'Sign in to GitHub' })).toBeInTheDocument();
  });

  it('shows a refused start verbatim', async () => {
    mockFetch({
      'GET /api/accounts': { body: accountsFixture() },
      'POST /api/accounts/github/device': {
        status: 422,
        body: { error: 'GitHub refused the sign-in: device flow disabled' },
      },
    });
    render(<AccountsTab client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Sign in to GitHub' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('device flow disabled');
  });

  it("saves the agent's credential without ever showing it, and removes it", async () => {
    let state = accountsFixture();
    const { calls } = mockFetch({
      'GET /api/accounts': () => ({ body: state }),
      'PUT /api/accounts/agent': () => {
        state = {
          ...state,
          agent: {
            ...state.agent,
            set: { claude_oauth_token: true },
            set_at: new Date().toISOString(),
            available: true,
          },
        };
        return { body: state };
      },
      'DELETE /api/accounts/agent': () => {
        state = { ...state, agent: { ...state.agent, set: {}, available: false } };
        return { body: state };
      },
    });
    render(<AccountsTab client={api()} />);
    const card = await screen.findByRole('region', { name: 'Agent' });
    expect(within(card).getByText(/has no credential/)).toBeInTheDocument();
    const field = within(card).getByLabelText('Claude subscription token');
    expect(field).toHaveAttribute('type', 'password');

    await userEvent.type(field, 'sk-ant oat');
    await userEvent.click(within(card).getAllByRole('button', { name: 'Save' })[0]);
    expect(within(card).getByRole('alert')).toHaveTextContent('one line');
    expect(lastCall(calls, 'PUT', '/api/accounts/agent')).toBeUndefined();

    await userEvent.clear(field);
    await userEvent.type(field, 'sk-ant-oat01-secret');
    await userEvent.click(within(card).getAllByRole('button', { name: 'Save' })[0]);
    expect(await within(card).findByText(/Saved. The next agent run uses it./)).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/accounts/agent')?.body).toEqual({
      kind: 'claude_oauth_token',
      value: 'sk-ant-oat01-secret',
    });
    expect(field).toHaveValue('');
    expect(within(card).getByText('Ready')).toBeInTheDocument();
    expect(card.textContent).not.toContain('sk-ant-oat01-secret');

    await userEvent.click(within(card).getByRole('button', { name: 'Remove' }));
    expect(await within(card).findByText('Removed.')).toBeInTheDocument();
    expect(lastCall(calls, 'DELETE', '/api/accounts/agent')?.url).toBe(
      '/api/accounts/agent?kind=claude_oauth_token',
    );
  });

  it("says the container's own credential wins", async () => {
    const a = accountsFixture();
    mockFetch({
      'GET /api/accounts': {
        body: { ...a, agent: { ...a.agent, available: true, container_credential: 'oauth_token' } },
      },
    });
    render(<AccountsTab client={api()} />);
    expect(await screen.findByText(/has a credential of its own/)).toBeInTheDocument();
  });
});
