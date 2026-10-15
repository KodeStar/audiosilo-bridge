import { describe, expect, it } from 'vitest';
import { appendLogs, formatAttrs, lastLogId, levelTone } from './logs';
import type { LogEntry } from '@/api/types';

const e = (id: number): LogEntry => ({
  id,
  time: '2026-10-15T10:00:00Z',
  level: 'INFO',
  message: `m${id}`,
});

describe('appendLogs', () => {
  it('appends new lines and skips repeats', () => {
    expect(appendLogs([e(1), e(2)], [e(2), e(3)]).map((x) => x.id)).toEqual([1, 2, 3]);
  });
  it('keeps the newest max lines', () => {
    expect(appendLogs([e(1), e(2)], [e(3), e(4)], 3).map((x) => x.id)).toEqual([2, 3, 4]);
  });
  it('returns the same list when nothing is new', () => {
    const shown = [e(1)];
    expect(appendLogs(shown, [])).toBe(shown);
    expect(appendLogs(shown, [e(1)])).toBe(shown);
  });
  it('lastLogId is the next after', () => {
    expect(lastLogId([])).toBe(0);
    expect(lastLogId([e(4), e(9)])).toBe(9);
  });
});

describe('formatAttrs', () => {
  it('sorts keys and quotes awkward values', () => {
    expect(formatAttrs({ b: '2', a: 'one two', c: '' })).toBe('a="one two" b=2 c=""');
    expect(formatAttrs(undefined)).toBe('');
  });
});

describe('levelTone', () => {
  it('colours warnings and errors', () => {
    expect(levelTone('ERROR')).toBe('bad');
    expect(levelTone('WARN')).toBe('warn');
    expect(levelTone('INFO')).toBe('muted');
  });
});
