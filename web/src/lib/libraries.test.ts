import { describe, expect, it } from 'vitest';
import { checkBadges, checkMountPath, mountSource } from './libraries';
import { libraryFixture } from '@/test/fixtures';

describe('libraries', () => {
  it('words where the mount comes from', () => {
    expect(mountSource(libraryFixture())).toMatch(/default/);
    expect(mountSource(libraryFixture({ configured: true }))).toBe('Set here');
  });
  it('badges a check', () => {
    expect(checkBadges(libraryFixture()).map((b) => b.label)).toEqual(['Mapped', 'Read only']);
    expect(checkBadges(libraryFixture({ status: null })).map((b) => b.label)).toEqual([
      'Not checked yet',
    ]);
    const bad = libraryFixture();
    bad.status = { ...bad.status!, mapped: false, writable: true };
    expect(checkBadges(bad).map((b) => b.label)).toEqual(['Not mapped', 'Writable']);
    expect(checkBadges(libraryFixture({ unknown: true })).map((b) => b.label)).toEqual([
      'Waiting for the server',
    ]);
    const slow = libraryFixture({ unknown: true, slow_at: '2026-11-19T10:00:00Z' });
    expect(checkBadges(slow).map((b) => b.label)).toEqual(['Waiting for the folder']);
    // A slow look that kept the earlier result badges that result.
    const kept = libraryFixture({ slow_at: '2026-11-19T10:00:00Z' });
    expect(checkBadges(kept).map((b) => b.label)).toEqual(['Mapped', 'Read only']);
  });
  it('wants an absolute path or nothing', () => {
    expect(checkMountPath('')).toBeNull();
    expect(checkMountPath('/mnt/books')).toBeNull();
    expect(checkMountPath('books')).not.toBeNull();
  });
});
