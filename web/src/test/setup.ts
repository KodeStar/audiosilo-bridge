import '@testing-library/jest-dom/vitest';
import { afterEach, vi } from 'vitest';
import { cleanup } from '@testing-library/react';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  try {
    localStorage.clear();
  } catch {
    // Ignore.
  }
  window.location.hash = '';
});
