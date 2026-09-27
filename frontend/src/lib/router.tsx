import { type AnchorHTMLAttributes, useMemo, useSyncExternalStore } from "react";

// A tiny history-based router: the panel has a handful of flat pages and
// does not need a routing library.

const listeners = new Set<() => void>();

function emit() {
  listeners.forEach((fn) => fn());
}

window.addEventListener("popstate", emit);

export function navigate(to: string) {
  if (to === location.pathname + location.search) return;
  history.pushState(null, "", to);
  emit();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, () => location.pathname);
}

/** The query string of the current URL, where pages keep their filters. */
export function useSearch(): URLSearchParams {
  const search = useSyncExternalStore(subscribe, () => location.search);
  return useMemo(() => new URLSearchParams(search), [search]);
}

/**
 * Replaces query parameters of the current URL without a new history entry,
 * so a filtered page can be linked to and survives a reload. Empty values
 * are removed.
 */
export function setSearch(params: Record<string, string | undefined>) {
  const next = new URLSearchParams(location.search);
  for (const [k, v] of Object.entries(params)) {
    if (v) next.set(k, v);
    else next.delete(k);
  }
  const qs = next.toString();
  const url = location.pathname + (qs ? `?${qs}` : "");
  if (url === location.pathname + location.search) return;
  history.replaceState(null, "", url);
  emit();
}

export function Link({ href, onClick, ...props }: AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) {
  return (
    <a
      href={href}
      onClick={(e) => {
        onClick?.(e);
        if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        e.preventDefault();
        navigate(href);
      }}
      {...props}
    />
  );
}
