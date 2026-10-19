import { describe, expect, it } from 'vitest';
import { DEFAULT_TAB, jobHash, routeFromHash } from './tabs';

describe('routeFromHash', () => {
  it('reads a known tab', () => {
    expect(routeFromHash('#jobs').tab).toBe('jobs');
    expect(routeFromHash('#/Logs').tab).toBe('logs');
  });
  it("reads the console's Accounts link", () => {
    expect(routeFromHash('#/accounts')).toEqual({ tab: 'accounts', jobId: null });
  });
  it('falls back to the status tab', () => {
    expect(routeFromHash('').tab).toBe(DEFAULT_TAB);
    expect(routeFromHash('#nope').tab).toBe('status');
  });
  it("reads the console's job deep link", () => {
    expect(routeFromHash('#/jobs/12')).toEqual({ tab: 'jobs', jobId: 12 });
    expect(routeFromHash(jobHash(345))).toEqual({ tab: 'jobs', jobId: 345 });
    expect(routeFromHash('#jobs/7')).toEqual({ tab: 'jobs', jobId: 7 });
  });
  it('takes no job from anything else', () => {
    expect(routeFromHash('#jobs')).toEqual({ tab: 'jobs', jobId: null });
    expect(routeFromHash('#/jobs/0')).toEqual({ tab: 'jobs', jobId: null });
    expect(routeFromHash('#/jobs/x1')).toEqual({ tab: 'jobs', jobId: null });
    expect(routeFromHash('#/jobs/-3')).toEqual({ tab: 'jobs', jobId: null });
    expect(routeFromHash('#/logs/12')).toEqual({ tab: 'logs', jobId: null });
    expect(routeFromHash('#/nope/12')).toEqual({ tab: DEFAULT_TAB, jobId: null });
  });
});
