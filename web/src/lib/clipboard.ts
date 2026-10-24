// Copying text from the page. The async Clipboard API needs a secure context
// (https or localhost), and the Bridge's page is usually plain http on a LAN
// address, so fall back to a hidden textarea and execCommand('copy'). Ported
// from the server's admin console (admin-ui/src/lib/clipboard.ts).

/** Copies text, returning whether it worked. */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Fall through to the legacy path.
  }
  const ta = document.createElement('textarea');
  try {
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    ta.style.opacity = '0';
    document.body.append(ta);
    ta.select();
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    ta.remove();
  }
}

/** Selects an element's text, so a copy the browser refused can be done by hand. */
export function selectText(el: Element): void {
  const sel = window.getSelection();
  if (!sel) return;
  const range = document.createRange();
  range.selectNodeContents(el);
  sel.removeAllRanges();
  sel.addRange(range);
}
