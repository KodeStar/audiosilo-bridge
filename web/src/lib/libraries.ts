import type { Library } from '@/api/types';
import type { Tone } from './jobs';

// mountSource words where a library's mount path comes from.
export function mountSource(lib: Library): string {
  return lib.configured ? 'Set here' : "The server's path (default)";
}

// checkBadges are the mapped and writable badges of a library's last check
// (none before its first check).
export function checkBadges(lib: Library): { label: string; tone: Tone }[] {
  const st = lib.status;
  if (!st) return [{ label: 'Not checked yet', tone: 'muted' }];
  return [
    st.mapped ? { label: 'Mapped', tone: 'good' } : { label: 'Not mapped', tone: 'bad' },
    st.writable ? { label: 'Writable', tone: 'good' } : { label: 'Read only', tone: 'warn' },
  ];
}

// checkMountPath returns an error sentence for a mount path the Bridge will
// refuse ("" is fine: use the server's root path).
export function checkMountPath(text: string): string | null {
  const t = text.trim();
  if (t === '' || t.startsWith('/')) return null;
  return 'The folder must be an absolute path, starting with /.';
}

// mountEditable: the environment (AUDIOSILO_BRIDGE_LIBRARIES) sets the mappings.
export function mountEditable(lib: Library): boolean {
  return !lib.locked;
}
