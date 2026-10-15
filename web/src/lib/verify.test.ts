import { describe, expect, it } from 'vitest';
import { parseVerifyResult, prettyJSON } from './verify';

describe('parseVerifyResult', () => {
  it('reads a full result', () => {
    const r = parseVerifyResult({
      library_id: 2,
      mount_path: '/books',
      path: 'A/B',
      files: [{ path: 'A/B/1.m4b', size: 10, expected_size: 10, ok: true }],
      writable: true,
      elapsed_ms: 42,
    });
    expect(r).toEqual({
      library_id: 2,
      mount_path: '/books',
      path: 'A/B',
      files: [{ path: 'A/B/1.m4b', size: 10, expected_size: 10, ok: true }],
      writable: true,
      elapsed_ms: 42,
    });
  });
  it('fills missing fields and skips bad file rows', () => {
    const r = parseVerifyResult({
      files: [
        { path: 'x.mp3', size: '12', ok: 'yes', error: 'not found here' },
        { size: 3 },
        null,
        'junk',
      ],
      writable: 'true',
      writable_error: 'permission denied',
    });
    expect(r?.files).toEqual([
      { path: 'x.mp3', size: 0, expected_size: 0, ok: false, error: 'not found here' },
    ]);
    expect(r?.writable).toBe(false);
    expect(r?.writable_error).toBe('permission denied');
    expect(r?.elapsed_ms).toBe(0);
  });
  it('is null without a files list', () => {
    expect(parseVerifyResult({})).toBeNull();
    expect(parseVerifyResult(null)).toBeNull();
    expect(parseVerifyResult([1])).toBeNull();
    expect(parseVerifyResult('done')).toBeNull();
  });
});

describe('prettyJSON', () => {
  it('indents', () => {
    expect(prettyJSON({ a: 1 })).toBe('{\n  "a": 1\n}');
  });
});
