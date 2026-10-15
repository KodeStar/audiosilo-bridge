import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { WorkersTab } from './WorkersTab';
import { ApiClient } from '@/api/client';
import { mockFetch } from '@/test/fetchMock';

describe('WorkersTab', () => {
  it('shows the empty state', async () => {
    mockFetch({ 'GET /api/workers': { body: { workers: [] } } });
    render(<WorkersTab client={new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() })} />);
    expect(
      await screen.findByText('ASR workers connect here in a later version.'),
    ).toBeInTheDocument();
  });
});
