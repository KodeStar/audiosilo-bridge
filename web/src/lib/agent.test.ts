import { describe, expect, it } from 'vitest';
import {
  checkBaseURL,
  chosenModels,
  endpointUpdate,
  missingModel,
  modelNote,
  modelWarnings,
  responsesWarning,
  serviceNote,
  stageLabel,
} from './agent';
import type { EndpointCheck } from '@/api/types';

function checkOf(over: Partial<EndpointCheck> = {}): EndpointCheck {
  return {
    models: [],
    responses: 'yes',
    responses_detail: '',
    error: '',
    key_sent: false,
    ...over,
  };
}

describe('checkBaseURL', () => {
  it('takes an http(s) address with a host', () => {
    for (const ok of [
      'http://192.168.1.20:11434/v1',
      'https://api.openai.com/v1',
      ' https://openrouter.ai/api/v1 ',
      'http://[fd00::1]:1234/v1',
    ]) {
      expect(checkBaseURL(ok)).toBeNull();
    }
  });
  it('refuses what the Bridge refuses', () => {
    for (const bad of [
      '',
      'ftp://h/v1',
      '192.168.1.20:11434',
      'http://u:p@h/v1',
      'http://h/v1?key=x',
      'http://h/v1#x',
      'http://h/v 1',
      'http://h/"v1',
    ]) {
      expect(checkBaseURL(bad)).not.toBeNull();
    }
  });
});

describe('endpoint models', () => {
  const stages = ['fact_pass', 'synthesizing', 'describing'];
  it('trims, drops blank stages and the trailing slash', () => {
    expect(
      endpointUpdate(' http://h/v1/ ', ' m ', { fact_pass: ' a ', synthesizing: ' ' }),
    ).toEqual({
      base_url: 'http://h/v1',
      model: 'm',
      models: { fact_pass: 'a' },
    });
  });
  it('needs a model for every stage', () => {
    expect(missingModel({ base_url: '', model: 'm', models: {} }, stages)).toBeNull();
    expect(missingModel({ base_url: '', model: '', models: { fact_pass: 'a' } }, stages)).toBe(
      'synthesizing',
    );
    expect(
      missingModel(
        { base_url: '', model: '', models: { fact_pass: 'a', synthesizing: 'a', describing: 'b' } },
        stages,
      ),
    ).toBeNull();
  });
  it('lists the models in use once each', () => {
    expect(
      chosenModels({ base_url: '', model: 'm', models: { synthesizing: 'big' } }, stages),
    ).toEqual(['m', 'big']);
  });
  it('labels stages, falling back to the id', () => {
    expect(stageLabel('fact_pass')).toBe('Fact notes');
    expect(stageLabel('new_stage')).toBe('new_stage');
  });
});

describe('warnings', () => {
  it('says nothing before a check, after a failed one, or for an endpoint listing nothing', () => {
    expect(modelWarnings(null, ['m'])).toEqual([]);
    expect(modelWarnings(checkOf({ error: 'x' }), ['m'])).toEqual([]);
    expect(modelWarnings(checkOf(), ['m'])).toEqual([]);
  });
  it('warns of a model not listed, without tools, or with a small context', () => {
    const check = checkOf({
      models: [
        { id: 'good', context_tokens: 131072, tools: true },
        { id: 'quiet' },
        { id: 'small', context_tokens: 8192, tools: false },
      ],
    });
    expect(modelWarnings(check, ['good', 'quiet'])).toEqual([]);
    const w = modelWarnings(check, ['small', 'gone']);
    expect(w).toHaveLength(3);
    expect(w[0]).toMatch(/small doesn't call tools/);
    expect(w[1]).toMatch(/small takes 8,192 tokens/);
    expect(w[2]).toBe("gone isn't among the endpoint's models.");
  });
  it('warns when the Responses API is missing or unknown', () => {
    expect(responsesWarning(checkOf())).toBeNull();
    expect(responsesWarning(null)).toBeNull();
    expect(responsesWarning(checkOf({ responses: 'no', responses_detail: 'no route' }))).toBe(
      'no route',
    );
    expect(responsesWarning(checkOf({ responses: 'unknown', responses_detail: '401' }))).toMatch(
      /couldn't tell .* \(401\)\.$/,
    );
  });
  it('notes what the endpoint says of a model', () => {
    expect(modelNote({ context_tokens: 40960, tools: true })).toBe('40,960 tokens, tools');
    expect(modelNote({})).toBe('');
  });
});

describe('serviceNote', () => {
  it('words a running service, and one not running with how to start it', () => {
    expect(serviceNote({ state: 'running', detail: '', found: '' })).toMatchObject({
      kind: 'ok',
      found: '',
      marker: null,
    });
    const off = serviceNote({
      state: 'not_running',
      detail: 'start it with docker compose --profile endpoint up -d agent-endpoint',
      found: '',
    });
    expect(off.kind).toBe('warn');
    expect(off.text).toContain('agent-endpoint');
    expect(off.text).toContain('--profile endpoint');
    expect(off.text).not.toMatch(/Claude|ChatGPT|agent container/);
    // An older Bridge's empty detail still says how.
    expect(serviceNote({ state: 'not_running', detail: '', found: '' }).text).toContain(
      '--profile endpoint',
    );
  });

  it('names what a refusing service found, and the marker file to remove', () => {
    const marker = serviceNote({
      state: 'refused',
      detail: "the endpoint's agent service holds a credential of its own",
      found: '/mnt/x holds /mnt/x/agent/.audiosilo-agent-home',
    });
    expect(marker.kind).toBe('error');
    expect(marker.found).toBe('/mnt/x holds /mnt/x/agent/.audiosilo-agent-home');
    expect(marker.marker).toContain('.audiosilo-agent-home');
    expect(marker.marker).toContain('delete');
    const variable = serviceNote({
      state: 'refused',
      detail: 'd',
      found: 'the variable OPENAI_API_KEY is set',
    });
    expect(variable.found).toBe('the variable OPENAI_API_KEY is set');
    expect(variable.marker).toBeNull();
  });
});
