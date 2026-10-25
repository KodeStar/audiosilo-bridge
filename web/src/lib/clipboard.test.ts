import { afterEach, describe, expect, it, vi } from 'vitest';
import { copyText, selectText } from './clipboard';

function stubSecure(secure: boolean, writeText?: (t: string) => Promise<void>) {
  vi.stubGlobal('isSecureContext', secure);
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: writeText ? { writeText } : undefined,
  });
}

function stubExec(result: boolean | 'throw') {
  const exec = vi.fn((cmd: string) => {
    if (result === 'throw') throw new Error('no');
    expect(cmd).toBe('copy');
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

  it('falls back to a hidden textarea on plain http', async () => {
    const writeText = vi.fn(async () => {});
    stubSecure(false, writeText);
    let copied = '';
    const exec = vi.fn(() => {
      copied = (document.activeElement as HTMLTextAreaElement | null)?.value ?? '';
      copied ||= document.querySelector('textarea')?.value ?? '';
      return true;
    });
    Object.defineProperty(document, 'execCommand', { configurable: true, value: exec });
    expect(await copyText('asw_token')).toBe(true);
    expect(writeText).not.toHaveBeenCalled();
    expect(copied).toBe('asw_token');
    expect(document.querySelector('textarea')).toBeNull();
  });

  it('falls back when the Clipboard API refuses', async () => {
    stubSecure(true, async () => {
      throw new Error('denied');
    });
    stubExec(true);
    expect(await copyText('x')).toBe(true);
  });

  it('says so when both ways fail', async () => {
    stubSecure(false);
    stubExec(false);
    expect(await copyText('x')).toBe(false);
    stubExec('throw');
    expect(await copyText('x')).toBe(false);
    expect(document.querySelector('textarea')).toBeNull();
  });
});

describe('copyText focus', () => {
  it('gives the focus back to what had it', async () => {
    stubSecure(false);
    stubExec(true);
    const button = document.createElement('button');
    document.body.append(button);
    button.focus();
    expect(await copyText('x')).toBe(true);
    expect(document.activeElement).toBe(button);
    button.remove();
  });
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
