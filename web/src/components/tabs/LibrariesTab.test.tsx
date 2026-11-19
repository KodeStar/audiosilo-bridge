import { describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LIBRARIES_POLL_MS, LibrariesTab } from './LibrariesTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { libraryFixture } from '@/test/fixtures';

const api = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

describe('LibrariesTab', () => {
  it('sets a mount path', async () => {
    const saved = libraryFixture({ mount_path: '/mnt/books', configured: true });
    const { calls } = mockFetch({
      'GET /api/libraries': { body: { libraries: [libraryFixture()] } },
      'PUT /api/libraries/1': { body: { libraries: [saved] } },
    });
    render(<LibrariesTab client={api()} />);

    expect(await screen.findByText('Audiobooks')).toBeInTheDocument();
    expect(screen.getByText('Mapped')).toBeInTheDocument();
    expect(screen.getByText('Read only')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Change folder' }));
    const input = screen.getByLabelText('Bridge folder');
    await userEvent.type(input, 'relative');
    await userEvent.click(screen.getByRole('button', { name: 'Save folder' }));
    expect(screen.getByRole('alert')).toHaveTextContent(/absolute path/);
    expect(lastCall(calls, 'PUT', '/api/libraries/1')).toBeUndefined();

    await userEvent.clear(input);
    await userEvent.type(input, '/mnt/books');
    await userEvent.click(screen.getByRole('button', { name: 'Save folder' }));

    expect(await screen.findByText('/mnt/books')).toBeInTheDocument();
    expect(screen.getByText('Set here')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/libraries/1')?.body).toEqual({ mount_path: '/mnt/books' });
  });

  it('names the checked marker of a podcast library', async () => {
    const podcasts = libraryFixture({
      library: { id: 2, name: 'Podcasts', root: '/podcasts', kind: 'podcasts' },
      status: {
        library_id: 2,
        mount_path: '/podcasts',
        mapped: true,
        writable: true,
        checked_path: '.audiosilo/library.json',
        error: '',
      },
    });
    mockFetch({ 'GET /api/libraries': { body: { libraries: [libraryFixture(), podcasts] } } });
    render(<LibrariesTab client={api()} />);
    expect(await screen.findByText('Checked marker')).toBeInTheDocument();
    expect(screen.getByText('Checked book')).toBeInTheDocument();
  });

  it('says when the last check got no answer and the results are the earlier ones', async () => {
    const kept = libraryFixture({ unanswered_at: new Date(Date.now() - 120_000).toISOString() });
    const answered = libraryFixture({
      library: { id: 2, name: 'More books', root: '/more', kind: 'books' },
    });
    mockFetch({ 'GET /api/libraries': { body: { libraries: [kept, answered] } } });
    render(<LibrariesTab client={api()} />);
    const notes = await screen.findAllByText(/The server gave no answer/);
    expect(notes).toHaveLength(1);
    expect(notes[0]).toHaveTextContent(
      'The server gave no answer (2 min ago). These are the results of the check before it. The Bridge will check again.',
    );
    // The earlier result still shows.
    expect(screen.getAllByText('Mapped')).toHaveLength(2);
  });

  it('says writes wait while no check stands', async () => {
    const lib = libraryFixture({
      unanswered_at: new Date(Date.now() - 120_000).toISOString(),
      unknown: true,
    });
    lib.status = { ...lib.status!, mapped: false, writable: false, error: 'no answer since then' };
    mockFetch({ 'GET /api/libraries': { body: { libraries: [lib] } } });
    render(<LibrariesTab client={api()} />);
    expect(await screen.findByText(/The server gave no answer/)).toHaveTextContent(
      'The server gave no answer (2 min ago). Writes wait for a check it answers. The Bridge will check again.',
    );
    expect(screen.getByText('Waiting for the server')).toBeInTheDocument();
    expect(screen.queryByText('Not mapped')).not.toBeInTheDocument();
  });

  it('says when the folder did not answer in time', async () => {
    const kept = libraryFixture({ slow_at: new Date(Date.now() - 120_000).toISOString() });
    const waiting = libraryFixture({
      library: { id: 2, name: 'More books', root: '/more', kind: 'books' },
      slow_at: new Date(Date.now() - 120_000).toISOString(),
      unknown: true,
    });
    waiting.status = {
      ...waiting.status!,
      mapped: false,
      error: 'the folder /more is not answering (no answer within 5s)',
    };
    mockFetch({ 'GET /api/libraries': { body: { libraries: [kept, waiting] } } });
    render(<LibrariesTab client={api()} />);
    const notes = await screen.findAllByText(/The folder did not answer within 5 s/);
    expect(notes.map((n) => n.textContent)).toEqual([
      'The folder did not answer within 5 s (2 min ago). These are the results of the check before it. The Bridge will look again.',
      'The folder did not answer within 5 s (2 min ago). Writes wait until it does. The Bridge will look again.',
    ]);
    expect(screen.getByText('Waiting for the folder')).toBeInTheDocument();
    expect(screen.getByText('Mapped')).toBeInTheDocument();
    expect(screen.queryByText('Waiting for the server')).not.toBeInTheDocument();
    expect(screen.queryByText(/The server gave no answer/)).not.toBeInTheDocument();
  });

  it('locks the folder when the environment sets it', async () => {
    mockFetch({
      'GET /api/libraries': { body: { libraries: [libraryFixture({ locked: true })] } },
    });
    render(<LibrariesTab client={api()} />);
    expect(
      await screen.findByText(/AUDIOSILO_BRIDGE_LIBRARIES sets the folders/),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Change folder' })).toBeNull();
  });

  it('checks now and shows the empty state', async () => {
    const { calls } = mockFetch({
      'GET /api/libraries': { body: { libraries: [libraryFixture()] } },
      'POST /api/libraries/check': { status: 202 },
    });
    render(<LibrariesTab client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    expect(await screen.findByText(/Checking/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/libraries/check')).toBeDefined();
    // The check runs in the background: no reload of the old results.
    expect(calls.filter((c) => c.url === '/api/libraries')).toHaveLength(1);
  });

  it('clears the checking note once the results are in', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let checked = false;
    mockFetch({
      'GET /api/libraries': () => ({
        body: {
          libraries: [libraryFixture(checked ? { checked_at: '2026-10-15T10:05:00Z' } : {})],
        },
      }),
      'POST /api/libraries/check': () => {
        checked = true;
        return { status: 202 };
      },
    });
    render(<LibrariesTab client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    expect(await screen.findByText(/Checking/)).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(LIBRARIES_POLL_MS + 100);
    });
    await waitFor(() => expect(screen.queryByText(/Checking/)).toBeNull());
  });

  it('says why there are no libraries', async () => {
    mockFetch({ 'GET /api/libraries': { body: { libraries: [] } } });
    render(<LibrariesTab client={api()} />);
    expect(await screen.findByText('No libraries yet')).toBeInTheDocument();
  });
});
