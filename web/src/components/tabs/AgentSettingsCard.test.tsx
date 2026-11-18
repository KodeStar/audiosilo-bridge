import { describe, expect, it, vi } from 'vitest';
import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AgentSettingsCard } from './AgentSettingsCard';
import { ApiClient } from '@/api/client';
import type { EndpointCheck } from '@/api/types';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { accountsFixture, agentSettingsFixture } from '@/test/fixtures';

function api() {
  return new ApiClient({ getToken: () => 't', onSignedOut: vi.fn(), onLanBlocked: vi.fn() });
}

describe('AgentSettingsCard', () => {
  it('shows the endpoint fields only for an endpoint, and saves the endpoint with its models', async () => {
    const { calls } = mockFetch({
      'GET /api/settings/agent': { body: agentSettingsFixture() },
      'PUT /api/settings/agent': ({ body }) => ({
        body: agentSettingsFixture({ ...(body as object), running_backend: 'claude' }),
      }),
    });
    render(<AgentSettingsCard client={api()} />);
    const select = await screen.findByLabelText('The agent runs');
    expect(screen.queryByLabelText('Endpoint address')).not.toBeInTheDocument();

    await userEvent.selectOptions(select, 'openai-compatible');
    const url = screen.getByLabelText('Endpoint address');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent("Enter the endpoint's address");
    await userEvent.type(url, 'http://user:pw@192.168.1.20:11434/v1');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Leave the user name and password out');
    await userEvent.clear(url);
    await userEvent.type(url, 'http://192.168.1.20:11434/v1/');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent('an endpoint has no default model');
    expect(lastCall(calls, 'PUT', '/api/settings/agent')).toBeUndefined();

    await userEvent.type(screen.getByLabelText('Model'), 'qwen3:32b');
    await userEvent.type(screen.getByLabelText('Fact notes'), ' qwen3:8b ');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/settings/agent')?.body).toEqual({
      backend: 'openai-compatible',
      openai_compatible: {
        base_url: 'http://192.168.1.20:11434/v1',
        model: 'qwen3:32b',
        models: { fact_pass: 'qwen3:8b' },
      },
    });
    // The backend applies when the Bridge restarts, and the page says so.
    expect(screen.getByText(/until the Bridge restarts/)).toBeInTheDocument();
  });

  it('tests the connection and warns about the chosen models, never blocking a save', async () => {
    const { calls } = mockFetch({
      'GET /api/settings/agent': {
        body: agentSettingsFixture({
          backend: 'openai-compatible',
          running_backend: 'openai-compatible',
          openai_compatible: {
            base_url: 'http://192.168.1.20:11434/v1',
            model: 'tiny',
            models: { synthesizing: 'gone' },
          },
        }),
      },
      'POST /api/settings/agent/test': {
        body: {
          models: [
            { id: 'qwen3:32b', context_tokens: 40960, tools: true },
            { id: 'tiny', context_tokens: 4096, tools: false },
          ],
          responses: 'no',
          responses_detail: 'the endpoint has no /responses (404)',
          error: '',
          key_sent: false,
        },
      },
      'PUT /api/settings/agent': ({ body }) => ({ body: agentSettingsFixture(body as object) }),
    });
    render(<AgentSettingsCard client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Test connection' }));
    expect(await screen.findByText(/answered with 2 models/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/settings/agent/test')?.body).toEqual({
      base_url: 'http://192.168.1.20:11434/v1',
    });
    const list = screen.getByRole('list', { name: "The endpoint's models" });
    expect(within(list).getByText('qwen3:32b')).toBeInTheDocument();
    expect(within(list).getByText('4,096 tokens, no tools')).toBeInTheDocument();
    expect(screen.getByText(/tiny doesn't call tools/)).toBeInTheDocument();
    expect(screen.getByText(/tiny takes 4,096 tokens of context/)).toBeInTheDocument();
    expect(screen.getByText("gone isn't among the endpoint's models.")).toBeInTheDocument();
    expect(screen.getByText('the endpoint has no /responses (404)')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();

    // Another address: the check was the old one's, so its models and warnings go.
    await userEvent.type(screen.getByLabelText('Endpoint address'), 'x');
    expect(screen.queryByRole('list', { name: "The endpoint's models" })).not.toBeInTheDocument();
    expect(screen.queryByText(/tiny doesn't call tools/)).not.toBeInTheDocument();
    expect(screen.queryByText('the endpoint has no /responses (404)')).not.toBeInTheDocument();
  });

  it("shows the endpoint's failure, and keeps the key write-only", async () => {
    const { calls } = mockFetch({
      'GET /api/settings/agent': {
        body: agentSettingsFixture({
          backend: 'openai-compatible',
          running_backend: 'openai-compatible',
          openai_compatible: {
            base_url: 'https://api.openai.com/v1',
            model: 'gpt-5.1',
            models: {},
          },
        }),
      },
      'POST /api/settings/agent/test': {
        body: {
          models: [],
          responses: '',
          responses_detail: '',
          error: 'the endpoint refused the API key (401 Unauthorized): check the key',
          key_sent: false,
        },
      },
      'PUT /api/accounts/agent': {
        body: accountsFixture({
          agent: {
            ...accountsFixture().agent,
            set: { endpoint_api_key: true },
            set_at_by_kind: { endpoint_api_key: '2026-11-18T09:00:00Z' },
          },
        }),
      },
    });
    render(<AgentSettingsCard client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Test connection' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('refused the API key');

    const key = screen.getByLabelText('Endpoint API key');
    expect(key).toHaveAttribute('type', 'password');
    expect(screen.getByText(/Not set/)).toBeInTheDocument();
    await userEvent.type(key, 'sk-proj-new');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    // With the address the page shows beside it: the Bridge refuses the key
    // when its saved address is another by now (a stale page).
    expect(lastCall(calls, 'PUT', '/api/accounts/agent')?.body).toEqual({
      kind: 'endpoint_api_key',
      value: 'sk-proj-new',
      endpoint: 'https://api.openai.com/v1',
    });
    expect(await screen.findByRole('button', { name: 'Replace' })).toBeInTheDocument();
    expect(key).toHaveValue('');
    // The model field kept what it held.
    expect(screen.getByLabelText('Model')).toHaveValue('gpt-5.1');
  });

  it('keeps the key to the saved address: no key form or key for another one, and a cleared key is said', async () => {
    const saved = {
      backend: 'openai-compatible' as const,
      running_backend: 'openai-compatible' as const,
      openai_compatible: { base_url: 'https://api.one.example/v1', model: 'm', models: {} },
      key_set: true,
      key_set_at: '2026-11-18T09:00:00Z',
    };
    const { calls } = mockFetch({
      'GET /api/settings/agent': { body: agentSettingsFixture(saved) },
      'POST /api/settings/agent/test': ({ body }) => ({
        body: {
          models: [{ id: 'm' }],
          responses: 'yes',
          responses_detail: '',
          error: '',
          // The Bridge sends the key to the saved address alone.
          key_sent: (body as { base_url: string }).base_url === 'https://api.one.example/v1',
        },
      }),
      // The Bridge removes the key with its address.
      'PUT /api/settings/agent': ({ body }) => ({
        body: agentSettingsFixture({
          ...saved,
          ...(body as object),
          key_set: false,
          key_set_at: '',
        }),
      }),
    });
    render(<AgentSettingsCard client={api()} />);
    // The saved address: tested with its key, which can be replaced here.
    await userEvent.click(await screen.findByRole('button', { name: 'Test connection' }));
    expect(await screen.findByText(/answered with 1 model/)).toBeInTheDocument();
    expect(screen.queryByText(/Tested without the API key/)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Endpoint API key')).toBeInTheDocument();

    // Another address in the field: no key form, and its test goes without the key.
    const url = screen.getByLabelText('Endpoint address');
    await userEvent.clear(url);
    await userEvent.type(url, 'https://api.two.example/v1');
    expect(screen.queryByLabelText('Endpoint API key')).not.toBeInTheDocument();
    expect(screen.getByText(/saving another address removes the key set now/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Test connection' }));
    expect(await screen.findByText(/Tested without the API key/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/settings/agent/test')?.body).toEqual({
      base_url: 'https://api.two.example/v1',
    });

    // Saved: the key went with the old address, and the page asks for this one's.
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(await screen.findByText(/was for the old address and is removed/)).toBeInTheDocument();
    // "Saving this address removes the key" has happened: it isn't said again.
    expect(screen.queryByText(/Tested without the API key/)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Endpoint API key')).toBeInTheDocument();
    expect(screen.getByText(/Not set/)).toBeInTheDocument();
  });

  it("drops a check's answer once the address was edited", async () => {
    mockFetch({
      'GET /api/settings/agent': {
        body: agentSettingsFixture({
          backend: 'openai-compatible',
          running_backend: 'openai-compatible',
          openai_compatible: { base_url: 'https://api.one.example/v1', model: 'm', models: {} },
        }),
      },
    });
    const client = api();
    let answer: (res: EndpointCheck) => void = () => {};
    vi.spyOn(client, 'checkEndpoint').mockImplementation(
      () => new Promise<EndpointCheck>((resolve) => (answer = resolve)),
    );
    render(<AgentSettingsCard client={client} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Test connection' }));
    expect(screen.getByRole('button', { name: 'Testing...' })).toBeDisabled();
    // The address changes while the check is out: its answer is the old one's.
    await userEvent.type(screen.getByLabelText('Endpoint address'), '/other');
    expect(screen.getByRole('button', { name: 'Test connection' })).toBeEnabled();
    await act(async () => {
      answer({
        models: [{ id: 'm' }],
        responses: 'yes',
        responses_detail: '',
        error: '',
        key_sent: false,
      });
    });
    expect(screen.queryByText(/answered with 1 model/)).not.toBeInTheDocument();
    expect(screen.queryByRole('list', { name: "The endpoint's models" })).not.toBeInTheDocument();
  });

  it('keeps settings the environment sets read-only', async () => {
    mockFetch({
      'GET /api/settings/agent': {
        body: agentSettingsFixture({
          backend: 'openai-compatible',
          running_backend: 'openai-compatible',
          openai_compatible: { base_url: 'http://10.0.0.5:1234/v1', model: 'm', models: {} },
          locked: ['agent.backend', 'agent.openai_compatible.base_url'],
        }),
      },
    });
    render(<AgentSettingsCard client={api()} />);
    expect(await screen.findByLabelText('The agent runs')).toBeDisabled();
    expect(screen.getByLabelText('Endpoint address')).toBeDisabled();
    expect(screen.getByLabelText('Model')).toBeEnabled();
  });
});
