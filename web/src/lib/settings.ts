import type { SettingKey, Settings } from '@/api/types';

// The settings keys the form edits, as the Bridge names them in Settings.locked.
export const KEY_PUBLIC_URL: SettingKey = 'public_url';
export const KEY_CHECK_MINUTES: SettingKey = 'mapping.check_minutes';

export const LOCKED_NOTE = 'Set by the environment';

export const MIN_CHECK_MINUTES = 1;
export const MAX_CHECK_MINUTES = 1440;

// isLocked: the environment sets this key, so the page can't change it.
export function isLocked(s: Pick<Settings, 'locked'> | null | undefined, key: SettingKey): boolean {
  return !!s && Array.isArray(s.locked) && s.locked.includes(key);
}

// allLocked: every field the form edits is locked (nothing to save).
export function allLocked(s: Settings): boolean {
  return isLocked(s, KEY_PUBLIC_URL) && isLocked(s, KEY_CHECK_MINUTES);
}

// parseCheckMinutes reads the interval field: the number, or an error sentence.
export function parseCheckMinutes(text: string): { value: number } | { error: string } {
  const t = text.trim();
  if (!/^\d+$/.test(t)) return { error: 'Enter a whole number of minutes.' };
  const n = Number(t);
  if (n < MIN_CHECK_MINUTES || n > MAX_CHECK_MINUTES) {
    return { error: `Enter between ${MIN_CHECK_MINUTES} and ${MAX_CHECK_MINUTES} minutes.` };
  }
  return { value: n };
}

// checkPublicURL returns an error sentence for a public URL the page can see is
// wrong ("" is fine: no link from the server's console).
export function checkPublicURL(text: string): string | null {
  const t = text.trim();
  if (t === '') return null;
  try {
    const u = new URL(t);
    if (u.protocol !== 'http:' && u.protocol !== 'https:') {
      return 'The address starts with http:// or https://.';
    }
    return null;
  } catch {
    return 'Enter a full address, like http://192.168.1.20:8091.';
  }
}
