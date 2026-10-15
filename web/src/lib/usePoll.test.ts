import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { usePoll } from './usePoll';

let hidden = false;

function setHidden(next: boolean) {
  hidden = next;
  document.dispatchEvent(new Event('visibilitychange'));
}

beforeEach(() => {
  hidden = false;
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
  vi.useFakeTimers();
});

afterEach(() => {
  // Drop the override: the prototype's getter shows through again.
  delete (document as { hidden?: boolean }).hidden;
});

// advance runs the timers and lets the awaited calls settle.
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

describe('usePoll', () => {
  it('calls at once and then every interval', async () => {
    const fn = vi.fn(async () => {});
    renderHook(() => usePoll(fn, 1000));
    await advance(0);
    expect(fn).toHaveBeenCalledTimes(1);
    await advance(1000);
    expect(fn).toHaveBeenCalledTimes(2);
    await advance(1000);
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it('calls once with a null interval', async () => {
    const fn = vi.fn(async () => {});
    renderHook(() => usePoll(fn, null));
    await advance(5000);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('skips ticks while the tab is hidden and calls once when it shows', async () => {
    const fn = vi.fn(async () => {});
    renderHook(() => usePoll(fn, 1000));
    await advance(0);
    expect(fn).toHaveBeenCalledTimes(1);

    act(() => setHidden(true));
    await advance(5000);
    expect(fn).toHaveBeenCalledTimes(1);

    act(() => setHidden(false));
    await advance(0);
    expect(fn).toHaveBeenCalledTimes(2);
    await advance(1000);
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it('waits for the tab to show before the first call', async () => {
    hidden = true;
    const fn = vi.fn(async () => {});
    renderHook(() => usePoll(fn, null));
    await advance(5000);
    expect(fn).not.toHaveBeenCalled();
    act(() => setHidden(false));
    await advance(0);
    expect(fn).toHaveBeenCalledTimes(1);
    // A once-only poll doesn't call again on a later return to the tab.
    act(() => setHidden(true));
    act(() => setHidden(false));
    await advance(5000);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('does not call twice when the tab shows before a tick is due', async () => {
    const fn = vi.fn(async () => {});
    renderHook(() => usePoll(fn, 1000));
    await advance(0);
    act(() => setHidden(true));
    act(() => setHidden(false));
    await advance(500);
    expect(fn).toHaveBeenCalledTimes(1);
    await advance(500);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('stops on unmount', async () => {
    const fn = vi.fn(async () => {});
    const { unmount } = renderHook(() => usePoll(fn, 1000));
    await advance(0);
    unmount();
    act(() => setHidden(true));
    act(() => setHidden(false));
    await advance(5000);
    expect(fn).toHaveBeenCalledTimes(1);
  });
});
