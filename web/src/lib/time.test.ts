import { describe, expect, it } from 'vitest';
import { bytes, dateTime, elapsed, parseTime, relativeTime } from './time';

const now = new Date('2026-10-15T12:00:00Z');

describe('parseTime', () => {
  it('reads the store nanosecond layout', () => {
    expect(parseTime('2026-10-15T11:59:00.123456789Z')?.toISOString()).toBe(
      '2026-10-15T11:59:00.123Z',
    );
  });
  it('reads RFC 3339 without a fraction', () => {
    expect(parseTime('2026-10-15T11:59:00Z')?.getTime()).toBe(Date.parse('2026-10-15T11:59:00Z'));
  });
  it('is null for empty or junk', () => {
    expect(parseTime('')).toBeNull();
    expect(parseTime(null)).toBeNull();
    expect(parseTime('yesterday')).toBeNull();
  });
});

describe('relativeTime', () => {
  it.each([
    ['2026-10-15T11:59:58Z', 'just now'],
    ['2026-10-15T11:59:20Z', '40 s ago'],
    ['2026-10-15T11:55:00Z', '5 min ago'],
    ['2026-10-15T09:00:00Z', '3 h ago'],
    ['2026-10-14T11:00:00Z', '1 day ago'],
    ['2026-10-12T12:00:00Z', '3 days ago'],
    ['2026-10-15T12:10:00Z', 'in 10 min'],
  ])('%s is %s', (t, want) => {
    expect(relativeTime(t, now)).toBe(want);
  });
  it('says never for no time', () => {
    expect(relativeTime(null, now)).toBe('never');
    expect(relativeTime('', now)).toBe('never');
  });
});

describe('dateTime', () => {
  it('is empty for no time and text otherwise', () => {
    expect(dateTime(null)).toBe('');
    expect(dateTime('2026-10-15T11:59:00Z')).not.toBe('');
  });
});

describe('bytes', () => {
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [1536, '1.5 KB'],
    [5 * 1024 * 1024, '5.0 MB'],
    [300 * 1024 * 1024, '300 MB'],
    [2 * 1024 ** 3, '2.0 GB'],
  ])('%d is %s', (n, want) => {
    expect(bytes(n)).toBe(want);
  });
  it('is empty for a negative size', () => {
    expect(bytes(-1)).toBe('');
  });
});

describe('elapsed', () => {
  it.each([
    [850, '850 ms'],
    [2400, '2.4 s'],
    [185000, '3 min 5 s'],
  ])('%d ms is %s', (ms, want) => {
    expect(elapsed(ms)).toBe(want);
  });
});
