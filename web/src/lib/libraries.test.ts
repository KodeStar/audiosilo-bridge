import { describe, expect, it } from 'vitest';
import { checkBadges, checkMountPath, mountEditable, mountSource } from './libraries';
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
  });
  it('wants an absolute path or nothing', () => {
    expect(checkMountPath('')).toBeNull();
    expect(checkMountPath('/mnt/books')).toBeNull();
    expect(checkMountPath('books')).not.toBeNull();
  });
  it('is not editable when the environment sets the mappings', () => {
    expect(mountEditable(libraryFixture())).toBe(true);
    expect(mountEditable(libraryFixture({ locked: true }))).toBe(false);
  });
});
