import type { Lane, Status } from '@/api/types';

// secretKeyText words where the master key that seals the server token came
// from.
export function secretKeyText(source: string): string {
  switch (source) {
    case 'environment':
      return 'From the environment (AUDIOSILO_BRIDGE_SECRET_KEY)';
    case 'key file':
      return 'From the key file in the data folder';
    case 'new key file':
      return 'A new key file was made in the data folder at this start. Back it up.';
    default:
      return source || 'Unknown';
  }
}

// laneText is "1 of 2 running".
export function laneText(l: Lane): string {
  return `${l.running} of ${l.capacity} running`;
}

// onlineText words the link to the server.
export function onlineText(s: Status): string {
  if (!s.pairing.paired) return 'Not paired';
  if (s.online) return 'Online';
  if (!s.last_heartbeat) return 'Waiting for the first heartbeat';
  return 'Offline';
}
