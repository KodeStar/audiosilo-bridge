import { useEffect, useRef } from 'react';

// usePoll calls fn now and then again interval ms after each call settles,
// until unmount. fn is read through a ref, so a new closure each render doesn't
// restart the loop; a new interval or restartKey does (calling fn at once). A
// null interval pauses polling.
export function usePoll(
  fn: () => Promise<unknown>,
  interval: number | null,
  restartKey?: unknown,
): void {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    if (interval === null) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      try {
        await ref.current();
      } catch {
        // The callee shows its own errors.
      }
      if (!stopped) timer = setTimeout(tick, interval);
    };
    void tick();
    return () => {
      stopped = true;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [interval, restartKey]);
}
