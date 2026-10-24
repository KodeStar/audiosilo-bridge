import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { WorkersTab } from './WorkersTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { statusFixture, workerFixture } from '@/test/fixtures';
import type { Status } from '@/api/types';

const client = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

// The copy tests stub document.execCommand: drop it, so no test inherits one.
afterEach(() => {
  Reflect.deleteProperty(document, 'execCommand');
});

// addWorker mocks a page that adds the worker "Studio" (token asw_secret).
function addWorker(status: Partial<Status> = {}) {
  return mockFetch({
    'GET /api/workers': { body: { workers: [] } },
    'GET /api/status': {
      body: statusFixture({ public_url: 'http://192.168.1.20:8091', ...status }),
    },
    'POST /api/workers': {
      status: 201,
      body: { worker: workerFixture({ id: 3, name: 'Studio' }), token: 'asw_secret' },
    },
  });
}

async function submitWorker() {
  await screen.findByText('No workers yet. Add one below.');
  // The status (worker_downloads) loads alongside: what the card shows from it
  // is awaited with findBy.
  await waitFor(() => expect(screen.getByLabelText('Name')).toBeInTheDocument());
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Studio' } });
  fireEvent.click(screen.getByRole('button', { name: 'Add worker' }));
  await screen.findByText(/shown only now/);
}

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
    const { calls } = addWorker();
    render(<WorkersTab client={client()} />);
    await screen.findByText('No workers yet. Add one below.');
    fireEvent.click(screen.getByRole('button', { name: 'Add worker' }));
    expect(await screen.findByText(/Give the worker a name/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Studio' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add worker' }));
    expect(
      await screen.findByText(
        'curl -fsSL http://192.168.1.20:8091/worker/install.sh | sh -s -- --token asw_secret',
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        '~/.audiosilo-bridge-worker/bin/audiosilo-bridge worker install --bridge http://192.168.1.20:8091 --token asw_secret',
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        '~/.audiosilo-bridge-worker/bin/audiosilo-bridge worker --bridge http://192.168.1.20:8091 --token asw_secret',
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Running an ASR worker' })).toHaveAttribute(
      'href',
      'https://docs.audiosilo.app/users/getting-started/running-a-worker',
    );
    expect(screen.getAllByRole('link', { name: 'the GitHub releases' })[0]).toHaveAttribute(
      'href',
      'https://github.com/KodeStar/audiosilo-bridge/releases',
    );
    expect(screen.getByText(/shown only now/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/workers')?.body).toEqual({ name: 'Studio' });
    fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByText(/asw_secret/)).not.toBeInTheDocument());
  });

  it('copies a command on plain http, through the fallback', async () => {
    addWorker();
    vi.stubGlobal('isSecureContext', false);
    const exec = vi.fn(() => true);
    Object.defineProperty(document, 'execCommand', { configurable: true, value: exec });
    render(<WorkersTab client={client()} />);
    await submitWorker();
    const copies = await screen.findAllByRole('button', { name: 'Copy' });
    expect(copies).toHaveLength(3);
    fireEvent.click(copies[0]);
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    expect(exec).toHaveBeenCalledWith('copy');
  });

  it('selects the command and says so when the browser will not copy', async () => {
    addWorker();
    vi.stubGlobal('isSecureContext', false);
    Object.defineProperty(document, 'execCommand', { configurable: true, value: () => false });
    render(<WorkersTab client={client()} />);
    await submitWorker();
    fireEvent.click((await screen.findAllByRole('button', { name: 'Copy' }))[0]);
    expect(await screen.findByText(/select and copy it/)).toBeInTheDocument();
    expect(window.getSelection()?.toString()).toBe(
      'curl -fsSL http://192.168.1.20:8091/worker/install.sh | sh -s -- --token asw_secret',
    );
    expect(screen.queryByRole('button', { name: 'Copied' })).not.toBeInTheDocument();
  });

  it('points at the releases when this Bridge has no worker downloads', async () => {
    addWorker({ worker_downloads: false });
    render(<WorkersTab client={client()} />);
    await submitWorker();
    expect(await screen.findByText(/has no worker downloads/)).toBeInTheDocument();
    expect(screen.queryByText(/curl -fsSL/)).not.toBeInTheDocument();
    expect(
      screen.getByText(/audiosilo-bridge worker install --bridge http:\/\/192.168.1.20:8091/),
    ).toBeInTheDocument();
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
