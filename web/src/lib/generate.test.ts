import { describe, expect, it } from 'vitest';
import {
  canRetry,
  chaptersSummary,
  checkedText,
  describeSummary,
  descriptionText,
  effectiveState,
  etaText,
  generateFacts,
  isSidecars,
  kindLabel,
  parkOf,
  parseChaptersResult,
  parseDescribeResult,
  parseGenerateResult,
  resultSummary,
  sourceText,
  usd,
} from './generate';
import { describeJobFixture, generateJobFixture, jobFixture } from '@/test/fixtures';

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

describe('descriptions', () => {
  it('labels the kinds and retries a parked description job', () => {
    expect(kindLabel('sidecars.describe')).toBe('Description');
    expect(kindLabel('sidecars.generate')).toBe('Characters and recaps');
    expect(kindLabel('bridge.verify')).toBe('');
    expect(kindLabel('constructor')).toBe('');
    expect(kindLabel('__proto__')).toBe('');
    expect(isSidecars(describeJobFixture())).toBe(true);
    expect(isSidecars(jobFixture())).toBe(false);
    expect(canRetry(describeJobFixture())).toBe(false);
    const parked = describeJobFixture({
      progress: { step: 'describing', park: { code: 'budget_exceeded', hint: 'Raise it.' } },
    });
    expect(canRetry(parked)).toBe(true);
  });
  it("words a generate result's description outcome", () => {
    const base = parseGenerateResult({ path: 'A/B', files: [] })!;
    expect(descriptionText(base)).toBe('');
    expect(descriptionText({ ...base, description: true })).toBe('With a description');
    expect(
      descriptionText(
        parseGenerateResult({
          path: 'A/B',
          files: [],
          description: false,
          description_error: 'the agent budget is spent',
        })!,
      ),
    ).toBe('No description: the agent budget is spent');
    expect(descriptionText({ ...base, description: false })).toBe('No description');
  });
  it('reads and words a description job result', () => {
    const r = parseDescribeResult({
      library_id: 1,
      path: 'Author/Book',
      description: true,
      files: ['description.json', 'bridge.json', 2],
      cost_usd: 0.25,
      checked_against: 'transcripts',
    });
    expect(r?.files).toEqual(['description.json', 'bridge.json']);
    expect(describeSummary(r!)).toBe('A description, $0.25, checked against the transcripts');
    expect(
      parseDescribeResult({ path: 'x', files: [], checked_against: 'nope' })?.checked_against,
    ).toBe('none');
    expect(checkedText('book text')).toBe('checked against the book text');
    expect(checkedText('none')).toBe('not checked: the source text was already removed');
    expect(parseDescribeResult(null)).toBeNull();
    expect(parseDescribeResult({ files: [] })).toBeNull();
  });
});

describe('chapters', () => {
  it('labels the kind and retries a parked chapters job', () => {
    const job = jobFixture({ kind: 'chapters.detect' });
    expect(kindLabel('chapters.detect')).toBe('Chapters');
    expect(isSidecars(job)).toBe(true);
    expect(canRetry(job)).toBe(false);
    const parked = jobFixture({
      kind: 'chapters.detect',
      state: 'running',
      progress: {
        step: 'chapter_detecting',
        park: { code: 'chapters_not_detected', hint: 'Add markers.' },
      },
    });
    expect(canRetry(parked)).toBe(true);
  });
  it('reads and words a chapters job result', () => {
    const r = parseChaptersResult({
      library_id: 1,
      path: 'Loose.m4b',
      chapters: 18,
      files: ['Loose.chapters.json', 'Loose.bridge.json', 3],
      source: 'audio',
    });
    expect(r?.files).toEqual(['Loose.chapters.json', 'Loose.bridge.json']);
    expect(chaptersSummary(r!)).toBe('18 chapters found in the narration');
    expect(chaptersSummary({ ...r!, chapters: 1, files: [] })).toBe(
      'One chapter: no chapter file was written',
    );
    expect(parseChaptersResult({ path: 'x', files: [] })).toBeNull();
    expect(parseChaptersResult(null)).toBeNull();
  });
});
