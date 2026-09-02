/**
 * A boolean that survives a reload — folded panels, collapsed rails.
 *
 * ONE IMPLEMENTATION BECAUSE THERE WERE ABOUT TO BE TWO. `RunTail` grew `useFolded` for its own
 * key, the nav rail needs exactly the same three lines for a different one, and a second copy is
 * how the two drift on the part that is not obvious: storage can throw on ACCESS, not only on the
 * value, in a browser with site data blocked — so both the read and the write need their own catch,
 * and a copy that only guards the write works everywhere except the machine that has the setting.
 *
 * FAILING TO PERSIST IS NOT AN ERROR. The fallback is the default state and one extra click on the
 * next reload; nothing here is worth a thrown exception in a render path.
 */
import { useCallback, useState } from 'react';

/** `true` iff storage holds `'1'`. Anything else — absent, `'0'`, garbage — is `false`. */
export function readFlag(key: string, storage?: Storage): boolean {
  try {
    return (storage ?? globalThis.localStorage)?.getItem(key) === '1';
  } catch {
    return false;
  }
}

/** Best-effort write. Returns whether it landed, which the tests assert and callers ignore. */
export function writeFlag(key: string, value: boolean, storage?: Storage): boolean {
  try {
    (storage ?? globalThis.localStorage)?.setItem(key, value ? '1' : '0');
    return true;
  } catch {
    return false;
  }
}

/** The hook form: `[flag, setFlag]`, remembered under `key`. */
export function usePersistedFlag(key: string): [boolean, (v: boolean) => void] {
  const [flag, set] = useState<boolean>(() => readFlag(key));
  const write = useCallback(
    (v: boolean) => {
      set(v);
      writeFlag(key, v);
    },
    [key]
  );
  return [flag, write];
}

/** Where the nav rail's collapsed state lives. Named here so a test cannot drift from the app. */
export const NAV_COLLAPSED_KEY = 'kontra.nav.collapsed';
