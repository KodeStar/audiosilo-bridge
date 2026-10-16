import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { WorkersTab } from './WorkersTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { statusFixture, workerFixture } from '@/test/fixtures';

const client = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

describe('WorkersTab', () => {
  it('lists the workers with what they run and did', async () => {
    mockFetch({
      'GET /api/workers': {
        body: {
          workers: [
            workerFixture(),
            workerFixture({
              id: 2,
              name: 'GPU box',
              connected: false,
              last_seen_at: '',
              segments: 0,
              backend: '',
            }),
          ],
        },
      },
      'GET /api/status': { body: statusFixture() },
    });
    render(<WorkersTab client={client()} />);
    expect(await screen.findByText('Mac Studio')).toBeInTheDocument();
    expect(screen.getByText('Connected')).toBeInTheDocument();
    expect(
      screen.getByText(/mlx-whisper on metal, darwin\/arm64 - version 0.2.0/),
    ).toBeInTheDocument();
    expect(screen.getByText(/49 segments, 11.1 h of audio, 20x real time/)).toBeInTheDocument();
    expect(screen.getByText('Never connected')).toBeInTheDocument();
  });

  it('adds a worker and shows its token once, in the commands', async () => {
    const { calls } = mockFetch({
      'GET /api/workers': { body: { workers: [] } },
      'GET /api/status': { body: statusFixture({ public_url: 'http://192.168.1.20:8091' }) },
      'POST /api/workers': {
        status: 201,
        body: { worker: workerFixture({ id: 3, name: 'Studio' }), token: 'asw_secret' },
      },
    });
    render(<WorkersTab client={client()} />);
    await screen.findByText('No workers yet. Add one below.');
    fireEvent.click(screen.getByRole('button', { name: 'Add worker' }));
    expect(await screen.findByText(/Give the worker a name/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Studio' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add worker' }));
    expect(
      await screen.findByText(
        'audiosilo-bridge worker install --bridge http://192.168.1.20:8091 --token asw_secret',
      ),
    ).toBeInTheDocument();
    expect(screen.getByText(/shown only now/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/workers')?.body).toEqual({ name: 'Studio' });
    fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByText(/asw_secret/)).not.toBeInTheDocument());
  });

  it('warns when the page is open at a local-only address', async () => {
    mockFetch({
      'GET /api/workers': { body: { workers: [] } },
      'GET /api/status': { body: statusFixture({ public_url: 'http://localhost:8091' }) },
      'POST /api/workers': { status: 201, body: { worker: workerFixture(), token: 'asw_x' } },
    });
    render(<WorkersTab client={client()} />);
    await screen.findByText('No workers yet. Add one below.');
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Mac' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add worker' }));
    expect(await screen.findByText(/local-only address/)).toBeInTheDocument();
  });

  it('removes a worker after a confirm', async () => {
    const { calls } = mockFetch({
      'GET /api/workers': { body: { workers: [workerFixture({ id: 7 })] } },
      'GET /api/status': { body: statusFixture() },
      'DELETE /api/workers/7': { status: 204 },
    });
    render(<WorkersTab client={client()} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Remove...' }));
    expect(screen.getByText(/revokes its token/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(lastCall(calls, 'DELETE', '/api/workers/7')).toBeDefined());
  });
});
