import { useEffect, useRef } from 'react';

// usePoll calls fn now and then again interval ms after each call settles,
// until unmount. fn is read through a ref, so a new closure each render doesn't
// restart the loop; a new interval or restartKey does (calling fn at once). A
// null interval calls fn once and stops there.
//
// While the browser tab is hidden a due call is skipped and the loop parks; it
// calls fn once when the tab shows again and carries on from there.
export function usePoll(
  fn: () => Promise<unknown>,
  interval: number | null,
  restartKey?: unknown,
): void {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    let stopped = false;
    let parked = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      timer = undefined;
      if (document.hidden) {
        parked = true;
        return;
      }
      try {
        await ref.current();
      } catch {
        // The callee shows its own errors.
      }
      if (!stopped && interval !== null) timer = setTimeout(tick, interval);
    };
    const onVisibility = () => {
      if (stopped || document.hidden || !parked) return;
      parked = false;
      void tick();
    };
    document.addEventListener('visibilitychange', onVisibility);
    void tick();
    return () => {
      stopped = true;
      document.removeEventListener('visibilitychange', onVisibility);
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [interval, restartKey]);
}
