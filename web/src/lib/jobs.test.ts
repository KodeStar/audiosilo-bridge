import { describe, expect, it } from 'vitest';
import {
  JOBS_POLL_BUSY_MS,
  JOBS_POLL_IDLE_MS,
  appendOlder,
  hasResult,
  jobTarget,
  jobsPollInterval,
  mergeJobs,
  progressFraction,
  progressText,
  reportText,
  stateHint,
  stateLabel,
  stateTone,
} from './jobs';
import { generateJobFixture, jobFixture } from '@/test/fixtures';

describe('state labels', () => {
  it('words every state', () => {
    expect(stateLabel('running')).toBe('Running');
    expect(stateLabel('needs_attention')).toBe('Needs attention');
    expect(stateLabel('lost')).toBe('Lost');
    expect(stateHint('lost')).toBe('The server took it back');
    expect(stateHint('done')).toBe('');
    expect(stateTone('failed')).toBe('bad');
    expect(stateTone('done')).toBe('good');
  });
  it('shows an unknown state as it is', () => {
    expect(stateLabel('paused')).toBe('paused');
    expect(stateTone('paused')).toBe('muted');
  });
});

describe('jobsPollInterval', () => {
  it('is fast while one runs and slow otherwise', () => {
    expect(jobsPollInterval([jobFixture({ state: 'done' }), jobFixture({ id: 8 })])).toBe(
      JOBS_POLL_BUSY_MS,
    );
    expect(jobsPollInterval([jobFixture({ state: 'done' })])).toBe(JOBS_POLL_IDLE_MS);
    expect(jobsPollInterval([])).toBe(JOBS_POLL_IDLE_MS);
  });
  it('is slow while the only running job is parked', () => {
    const parked = generateJobFixture({
      progress: { step: 'fact_pass', park: { code: 'core_needed', hint: '' } },
    });
    expect(jobsPollInterval([parked])).toBe(JOBS_POLL_IDLE_MS);
  });
});

describe('reportText', () => {
  it('flags a finished job the server has not heard about', () => {
    expect(reportText(jobFixture({ state: 'done', reported: false }))).toBe('Report pending');
    expect(reportText(jobFixture({ state: 'failed', reported: true }))).toBe('Reported');
    expect(reportText(jobFixture({ state: 'running' }))).toBe('');
    expect(reportText(jobFixture({ state: 'lost' }))).toBe('');
  });
});

describe('progress', () => {
  it('words a step, a message and a count', () => {
    expect(progressText({ step: 'stat', message: 'Comparing', done: 3, total: 12 })).toBe(
      'stat: Comparing (3/12)',
    );
    expect(progressText({ step: 'lookup' })).toBe('lookup');
    expect(progressText({ step: 'stat', total: 4 })).toBe('stat (0/4)');
    expect(progressText(null)).toBe('');
    expect(progressText({})).toBe('');
  });
  it('has a fraction only with a total', () => {
    expect(progressFraction({ done: 1, total: 4 })).toBe(0.25);
    expect(progressFraction({ done: 9, total: 4 })).toBe(1);
    expect(progressFraction({ step: 'x' })).toBeNull();
  });
});

describe('jobTarget', () => {
  it('joins the library and the path', () => {
    expect(jobTarget(jobFixture())).toBe('library 1: Author/Book');
    expect(jobTarget(jobFixture({ path: null }))).toBe('library 1');
    expect(jobTarget(jobFixture({ library_id: null, path: null }))).toBe('');
  });
});

describe('mergeJobs and appendOlder', () => {
  it('lets the fresh page win and keeps older pages', () => {
    const shown = [jobFixture({ id: 9 }), jobFixture({ id: 8 }), jobFixture({ id: 3 })];
    const fresh = [
      jobFixture({ id: 10 }),
      jobFixture({ id: 9, state: 'done' }),
      jobFixture({ id: 8 }),
    ];
    const merged = mergeJobs(fresh, shown);
    expect(merged.map((j) => j.id)).toEqual([10, 9, 8, 3]);
    expect(merged[1].state).toBe('done');
  });
  it('is empty when the first page is', () => {
    expect(mergeJobs([], [jobFixture()])).toEqual([]);
  });
  it('appends older jobs without repeats', () => {
    const out = appendOlder(
      [jobFixture({ id: 9 })],
      [jobFixture({ id: 9 }), jobFixture({ id: 4 })],
    );
    expect(out.map((j) => j.id)).toEqual([9, 4]);
  });
});

describe('hasResult', () => {
  it('is false for an empty object or null', () => {
    expect(hasResult(jobFixture({ result: {} }))).toBe(false);
    expect(hasResult(jobFixture({ result: null }))).toBe(false);
    expect(hasResult(jobFixture({ result: { files: [] } }))).toBe(true);
  });
});
