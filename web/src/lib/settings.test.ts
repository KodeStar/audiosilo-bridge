import { describe, expect, it } from 'vitest';
import {
  KEY_CHECK_MINUTES,
  KEY_PUBLIC_URL,
  allLocked,
  checkPublicURL,
  isLocked,
  parseCheckMinutes,
} from './settings';
import { settingsFixture } from '@/test/fixtures';

describe('isLocked', () => {
  it('reads the locked list', () => {
    const s = settingsFixture({ locked: ['public_url'] });
    expect(isLocked(s, KEY_PUBLIC_URL)).toBe(true);
    expect(isLocked(s, KEY_CHECK_MINUTES)).toBe(false);
    expect(isLocked(null, KEY_PUBLIC_URL)).toBe(false);
  });
  it('allLocked needs every field locked', () => {
    expect(allLocked(settingsFixture({ locked: ['public_url'] }))).toBe(false);
    expect(allLocked(settingsFixture({ locked: ['public_url', 'mapping.check_minutes'] }))).toBe(
      false,
    );
    expect(
      allLocked(
        settingsFixture({ locked: ['public_url', 'mapping.check_minutes', 'asr.worker_fallback'] }),
      ),
    ).toBe(true);
  });
});

describe('parseCheckMinutes', () => {
  it('accepts 1 to 1440', () => {
    expect(parseCheckMinutes('1')).toEqual({ value: 1 });
    expect(parseCheckMinutes(' 1440 ')).toEqual({ value: 1440 });
  });
  it('refuses out of range and non-numbers', () => {
    expect(parseCheckMinutes('0')).toHaveProperty('error');
    expect(parseCheckMinutes('1441')).toHaveProperty('error');
    expect(parseCheckMinutes('2.5')).toHaveProperty('error');
    expect(parseCheckMinutes('')).toHaveProperty('error');
  });
});

describe('checkPublicURL', () => {
  it('accepts empty and http(s) addresses', () => {
    expect(checkPublicURL('')).toBeNull();
    expect(checkPublicURL('http://192.168.1.20:8091')).toBeNull();
    expect(checkPublicURL('https://bridge.lan')).toBeNull();
  });
  it('refuses other text', () => {
    expect(checkPublicURL('bridge.lan')).not.toBeNull();
    expect(checkPublicURL('ftp://bridge.lan')).not.toBeNull();
  });
});
