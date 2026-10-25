import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { copyText, selectText } from './clipboard';

function stubSecure(secure: boolean, writeText?: (t: string) => Promise<void>) {
  vi.stubGlobal('isSecureContext', secure);
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: writeText ? { writeText } : undefined,
  });
}

// onCopy runs inside execCommand, while the hidden textarea is in the page.
function stubExec(result: boolean | 'throw', onCopy?: () => void) {
  const exec = vi.fn((cmd: string) => {
    expect(cmd).toBe('copy');
    onCopy?.();
    if (result === 'throw') throw new Error('no');
    return result;
  });
  Object.defineProperty(document, 'execCommand', { configurable: true, value: exec });
  return exec;
}

afterEach(() => {
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
  Reflect.deleteProperty(document, 'execCommand');
});

describe('copyText', () => {
  it('uses the Clipboard API in a secure context', async () => {
    const writeText = vi.fn(async () => {});
    stubSecure(true, writeText);
    const exec = stubExec(false);
    expect(await copyText('curl x | sh')).toBe(true);
    expect(writeText).toHaveBeenCalledWith('curl x | sh');
    expect(exec).not.toHaveBeenCalled();
  });

  it('falls back when the Clipboard API refuses', async () => {
    stubSecure(true, async () => {
      throw new Error('denied');
    });
    stubExec(true);
    expect(await copyText('x')).toBe(true);
  });
});

describe('copyText fallback on plain http', () => {
  let button: HTMLButtonElement;
  let popup: HTMLElement | undefined;

  beforeEach(() => {
    stubSecure(false);
    button = document.createElement('button');
    document.body.append(button);
    button.focus();
  });

  afterEach(() => {
    button.remove();
    popup?.remove();
    popup = undefined;
  });

  it('copies the selected text from a hidden textarea and gives the focus back', async () => {
    const writeText = vi.fn(async () => {});
    stubSecure(false, writeText);
    let copied = '';
    stubExec(true, () => {
      const ta = document.activeElement as HTMLTextAreaElement;
      copied = ta.value.slice(ta.selectionStart, ta.selectionEnd);
    });
    expect(await copyText('asw_token')).toBe(true);
    expect(writeText).not.toHaveBeenCalled();
    expect(copied).toBe('asw_token');
    expect(document.querySelector('textarea')).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('places the textarea beside the focused element, inside its dialog', async () => {
    popup = document.createElement('div');
    popup.setAttribute('role', 'dialog');
    document.body.append(popup);
    popup.append(button);
    button.focus();
    let parent: Element | null = null;
    stubExec(true, () => {
      parent = document.activeElement?.parentElement ?? null;
    });
    expect(await copyText('x')).toBe(true);
    expect(parent).toBe(popup);
    expect(document.activeElement).toBe(button);
  });

  it.each([
    ['refuses', false],
    ['throws', 'throw'],
  ] as const)(
    'says so, removes the textarea and restores focus when execCommand %s',
    async (_, result) => {
      stubExec(result);
      expect(await copyText('x')).toBe(false);
      expect(document.querySelector('textarea')).toBeNull();
      expect(document.activeElement).toBe(button);
    },
  );
});

describe('selectText', () => {
  it('selects the element text', () => {
    const el = document.createElement('code');
    el.textContent = 'curl -fsSL http://x/worker/install.sh';
    document.body.append(el);
    selectText(el);
    expect(window.getSelection()?.toString()).toBe('curl -fsSL http://x/worker/install.sh');
    el.remove();
  });
});
