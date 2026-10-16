import { describe, expect, it } from 'vitest';
import {
  canRetry,
  effectiveState,
  etaText,
  generateFacts,
  parkOf,
  parseGenerateResult,
  resultSummary,
  sourceText,
  usd,
} from './generate';
import { generateJobFixture, jobFixture } from '@/test/fixtures';

const parked = () =>
  generateJobFixture({
    error: 'agent backend unavailable',
    progress: {
      step: 'fact_pass',
      stage: 'fact_pass',
      source: 'audio',
      cost_usd: 1,
      park: {
        code: 'agent_unavailable',
        hint: 'Check the agent container.',
        retry_at: '2026-10-15T12:00:00Z',
      },
    },
  });

describe('parks', () => {
  it('shows a parked running job as needing attention, retryable', () => {
    const job = parked();
    expect(parkOf(job)?.code).toBe('agent_unavailable');
    expect(effectiveState(job)).toBe('needs_attention');
    expect(canRetry(job)).toBe(true);
  });
  it('leaves a running, a finished and a verify job alone', () => {
    expect(effectiveState(generateJobFixture())).toBe('running');
    expect(canRetry(generateJobFixture())).toBe(false);
    const finished = { ...parked(), state: 'done' };
    expect(parkOf(finished)).toBeNull();
    expect(effectiveState(finished)).toBe('done');
    expect(canRetry(jobFixture({ progress: { step: 'x', park: { code: 'x', hint: '' } } }))).toBe(
      false,
    );
  });
});

describe('wording', () => {
  it('words ETAs, money and sources', () => {
    expect(etaText(30)).toBe('under a minute');
    expect(etaText(25 * 60)).toBe('about 25 min');
    expect(etaText(3 * 3600)).toBe('about 3 h');
    expect(etaText(3 * 3600 + 600)).toBe('about 3 h 10 min');
    expect(etaText(52 * 3600)).toBe('about 2 days 4 h');
    expect(etaText(undefined)).toBe('');
    expect(etaText(-1)).toBe('');
    expect(usd(3.4)).toBe('$3.40');
    expect(usd(undefined)).toBe('');
    expect(sourceText('ebook')).toBe('the ebook');
    expect(sourceText('video')).toBe('');
  });
  it('lists the facts of a running job, no ETA while parked', () => {
    expect(generateFacts(generateJobFixture().progress, false)).toEqual([
      'about 1 h 30 min left',
      '$3.42 spent',
      'from the audio',
    ]);
    expect(generateFacts(generateJobFixture().progress, true)).toEqual([
      '$3.42 spent',
      'from the audio',
    ]);
    expect(generateFacts(null, false)).toEqual([]);
  });
});

describe('parseGenerateResult', () => {
  it('reads a result and words it', () => {
    const r = parseGenerateResult({
      library_id: 1,
      path: 'Author/Book',
      source: 'audio',
      files: ['characters.json', 'recaps.json', 'bridge.json', 4],
      characters: 41,
      recaps: 1,
      cost_usd: 12.5,
      work_id: 'the-book',
      contributed: { characters: 'https://github.com/x/y/issues/1', recaps: 'javascript:alert(1)' },
    });
    expect(r?.files).toEqual(['characters.json', 'recaps.json', 'bridge.json']);
    expect(r?.contributed).toEqual({ characters: 'https://github.com/x/y/issues/1' });
    expect(resultSummary(r!)).toBe('41 characters and 1 recap from the audio, $12.50');
  });
  it('refuses anything else', () => {
    expect(parseGenerateResult(null)).toBeNull();
    expect(parseGenerateResult({ files: [] })).toBeNull();
    expect(parseGenerateResult('x')).toBeNull();
  });
});
