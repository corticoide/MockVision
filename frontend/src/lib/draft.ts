import { useState } from "react";

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/**
 * useDraft holds the edited copy of saved values. When the saved values
 * change (a save here, or a change elsewhere arriving live), an untouched
 * draft follows them and an edited one is kept, so nobody loses what they
 * typed and the form never has to be remounted.
 */
export function useDraft<T>(saved: T) {
  const key = JSON.stringify(saved);
  const [state, setState] = useState({ key, base: saved, draft: saved });
  let current = state;
  if (state.key !== key) {
    current = { key, base: saved, draft: same(state.draft, state.base) ? saved : state.draft };
    setState(current);
  }
  return {
    draft: current.draft,
    dirty: !same(current.draft, current.base),
    set: (patch: Partial<T>) => setState((s) => ({ ...s, draft: { ...s.draft, ...patch } })),
    update: (fn: (draft: T) => T) => setState((s) => ({ ...s, draft: fn(s.draft) })),
    discard: () => setState((s) => ({ ...s, draft: s.base })),
    /** Starts again from values just saved. */
    resetTo: (values: T) => setState({ key: JSON.stringify(values), base: values, draft: values }),
  };
}
