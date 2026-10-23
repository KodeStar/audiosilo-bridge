import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LibrariesTab } from './LibrariesTab';
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

  it('says why there are no libraries', async () => {
    mockFetch({ 'GET /api/libraries': { body: { libraries: [] } } });
    render(<LibrariesTab client={api()} />);
    expect(await screen.findByText('No libraries yet')).toBeInTheDocument();
  });
});
