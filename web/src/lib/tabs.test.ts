import { describe, expect, it } from 'vitest';
import { DEFAULT_TAB, tabFromHash } from './tabs';

describe('tabFromHash', () => {
  it('reads a known tab', () => {
    expect(tabFromHash('#jobs')).toBe('jobs');
    expect(tabFromHash('#/Logs')).toBe('logs');
  });
  it('falls back to the status tab', () => {
    expect(tabFromHash('')).toBe(DEFAULT_TAB);
    expect(tabFromHash('#nope')).toBe('status');
  });
});
