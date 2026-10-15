import { describe, expect, it } from 'vitest';
import { laneText, onlineText, secretKeyText } from './status';
import { pairedStatus, statusFixture } from '@/test/fixtures';

describe('secretKeyText', () => {
  it('words each source', () => {
    expect(secretKeyText('environment')).toMatch(/environment \(AUDIOSILO_BRIDGE_SECRET_KEY\)/);
    expect(secretKeyText('key file')).toMatch(/key file/);
    expect(secretKeyText('new key file')).toMatch(/Back it up/);
    expect(secretKeyText('vault')).toBe('vault');
    expect(secretKeyText('')).toBe('Unknown');
  });
});

describe('laneText', () => {
  it('is running of capacity', () => {
    expect(laneText({ name: 'light', capacity: 2, running: 1 })).toBe('1 of 2 running');
  });
});

describe('onlineText', () => {
  it('covers unpaired, online, waiting and offline', () => {
    expect(onlineText(statusFixture())).toBe('Not paired');
    expect(onlineText(pairedStatus())).toBe('Online');
    expect(onlineText(pairedStatus({ online: false, last_heartbeat: null }))).toBe(
      'Waiting for the first heartbeat',
    );
    expect(onlineText(pairedStatus({ online: false }))).toBe('Offline');
  });
});
