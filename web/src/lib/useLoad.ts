import { useCallback, useRef, useState } from 'react';
import { errorText } from '@/api/client';
import { usePoll } from './usePoll';

export const LOAD_ERROR = "Can't reach the Bridge.";

interface LoadOptions<T> {
  // The error shown when the fetch fails without the Bridge's own sentence.
  fallback?: string;
  // Folds a fresh answer into the data shown (default: the fresh answer).
  merge?: (fresh: T, shown: T | null) => T;
}

// useLoad fetches data now and every interval ms (null: once), keeping the
// last good answer and the last error. A good answer clears the error. The
// interval can be worked out from the data shown. fetchFn and merge are read
// fresh each call, so they needn't be memoised. reload fetches at once,
// outside the poll.
export function useLoad<T>(
  fetchFn: () => Promise<T>,
  interval: number | null | ((shown: T | null) => number | null),
  { fallback = LOAD_ERROR, merge }: LoadOptions<T> = {},
) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const latest = useRef({ fetchFn, merge, fallback });
  latest.current = { fetchFn, merge, fallback };

  const reload = useCallback(async () => {
    const { fetchFn, merge, fallback } = latest.current;
    try {
      const fresh = await fetchFn();
      setData((shown) => (merge ? merge(fresh, shown) : fresh));
      setError(null);
    } catch (err) {
      setError(errorText(err, fallback));
    }
  }, []);
  usePoll(reload, typeof interval === 'function' ? interval(data) : interval);

  return { data, setData, error, setError, reload };
}
