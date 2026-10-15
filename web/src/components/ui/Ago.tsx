import { dateTime, relativeTime } from '@/lib/time';

// Ago words how long ago a time was, with the date and time as its tooltip.
export function Ago({ at }: { at: string }) {
  return <span title={dateTime(at)}>{relativeTime(at)}</span>;
}
