import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { App } from './App';
import { mockFetch } from '@/test/fetchMock';
import { pairedStatus } from '@/test/fixtures';
import { TOKEN_KEY } from '@/lib/token';

describe('App', () => {
  it('signs in, keeps the token and shows the status', async () => {
    const { calls } = mockFetch({
      'POST /api/login': ({ body }) =>
        (body as { password: string }).password === 'secret'
          ? { body: { token: 'tok-1' } }
          : { status: 401, body: { error: 'wrong password' } },
      'GET /api/status': { body: pairedStatus() },
    });
    render(<App />);
    expect(
      screen.getByText(/printed in the Bridge's log the first time it started/),
    ).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText('Password'), 'nope');
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Wrong password.');

    await userEvent.clear(screen.getByLabelText('Password'));
    await userEvent.type(screen.getByLabelText('Password'), 'secret');
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }));

    expect(await screen.findByText('Home')).toBeInTheDocument();
    expect(localStorage.getItem(TOKEN_KEY)).toBe('tok-1');
    const statusCall = calls.find((c) => c.url === '/api/status');
    expect(statusCall?.headers.Authorization).toBe('Bearer tok-1');
  });

  it('returns to sign in on a 401', async () => {
    localStorage.setItem(TOKEN_KEY, 'dead');
    mockFetch({ 'GET /api/status': { status: 401, body: { error: 'sign in first' } } });
    render(<App />);
    expect(await screen.findByLabelText('Password')).toBeInTheDocument();
    await waitFor(() => expect(localStorage.getItem(TOKEN_KEY)).toBeNull());
  });

  it('shows the LAN guard message on a 403', async () => {
    localStorage.setItem(TOKEN_KEY, 'tok');
    mockFetch({
      'GET /api/status': { status: 403, body: { error: 'lan only', code: 'lan_only' } },
    });
    render(<App />);
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This page answers only the local network.',
    );
  });

  it('signs out from the header', async () => {
    localStorage.setItem(TOKEN_KEY, 'tok');
    const { calls } = mockFetch({
      'GET /api/status': { body: pairedStatus() },
      'POST /api/logout': { status: 204 },
    });
    render(<App />);
    await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    expect(await screen.findByLabelText('Password')).toBeInTheDocument();
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(calls.some((c) => c.method === 'POST' && c.url === '/api/logout')).toBe(true);
  });

  it('opens the tab named in the hash and switches tabs', async () => {
    localStorage.setItem(TOKEN_KEY, 'tok');
    window.location.hash = '#workers';
    mockFetch({
      'GET /api/workers': { body: { workers: [] } },
      'GET /api/jobs': { body: { jobs: [] } },
    });
    render(<App />);
    expect(await screen.findByText('No workers yet. Add one below.')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('tab', { name: 'Jobs' }));
    expect(await screen.findByText('No jobs yet')).toBeInTheDocument();
    expect(window.location.hash).toBe('#jobs');
  });
});
