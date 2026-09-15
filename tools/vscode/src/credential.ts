/**
 * Whether a clipboard's contents plausibly ARE the credential, and may pre-fill the password box.
 *
 * WHY PRE-FILL AT ALL. `kontra init` prints the console password once and the only sane gesture is
 * to copy it. But the Connect prompt is the editor's own QuickInput — not a webview, not ours — and
 * on Cursor the paste keystroke does not reach it. The alternative is typing twenty random
 * characters by hand, which is how a one-time credential gets mistyped twice and then written down
 * somewhere worse than the clipboard.
 *
 * NARROW ON PURPOSE, because the box is MASKED: whatever lands there is invisible, and a value the
 * operator did not choose must not be silently submitted to a server as a password guess. A
 * credential has no whitespace and is not a paragraph, so ordinary copied text — a path, a
 * sentence, a stack trace, a multi-line snippet — is ignored and the box opens empty, exactly as
 * before. The prompt says when it was filled, so a masked box is never a surprise.
 */
export function looksLikeCredential(text: string): boolean {
  const value = text.trim();
  return value !== '' && value.length <= 128 && !/\s/.test(value);
}

/** The clipboard text to pre-fill with, or '' to leave the box empty. */
export function credentialPrefill(clipboard: string): string {
  const value = clipboard.trim();
  return looksLikeCredential(value) ? value : '';
}
