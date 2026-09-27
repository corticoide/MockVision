import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";
import { es } from "./i18n.es";
import { setFormatLocale } from "./utils";

export type Locale = "en" | "es";

/** Languages offered in the selector, each in its own endonym. */
export const locales: { id: Locale; name: string }[] = [
  { id: "en", name: "English" },
  { id: "es", name: "Español" },
];

const dictionaries: Record<Locale, Record<string, string>> = { en: {}, es };
const STORAGE_KEY = "mockvision.locale";

function detect(): Locale {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "en" || saved === "es") return saved;
  } catch {
    // localStorage may be blocked; fall back to the browser language.
  }
  const lang = typeof navigator !== "undefined" ? navigator.language : "en";
  return lang.toLowerCase().startsWith("es") ? "es" : "en";
}

export type Vars = Record<string, string | number>;

function interpolate(template: string, vars?: Vars): string {
  if (!vars) return template;
  return template.replace(/\{(\w+)\}/g, (m, key) => (key in vars ? String(vars[key]) : m));
}

export type Translate = (key: string, vars?: Vars) => string;

interface I18n {
  locale: Locale;
  setLocale: (locale: Locale) => void;
  t: Translate;
}

// The initial locale is resolved once, before the first render, so numbers
// and dates are formatted in the right language from the start.
const initialLocale = detect();
setFormatLocale(initialLocale);

const I18nContext = createContext<I18n | null>(null);

export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(initialLocale);

  // Keep number/date formatting in step with the labels within the same render:
  // this runs before the consumers below re-render, so formatBytes and friends
  // already use the new locale. It is idempotent (guarded in setFormatLocale).
  setFormatLocale(locale);

  useEffect(() => {
    document.documentElement.lang = locale;
  }, [locale]);

  const setLocale = useCallback((next: Locale) => {
    setLocaleState(next);
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      // Not persisting the choice is acceptable; it still applies this session.
    }
  }, []);

  const t = useCallback<Translate>(
    (key, vars) => interpolate(dictionaries[locale][key] ?? key, vars),
    [locale],
  );

  const value = useMemo<I18n>(() => ({ locale, setLocale, t }), [locale, setLocale, t]);
  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18n {
  const ctx = useContext(I18nContext);
  if (!ctx) throw new Error("useI18n must be used within I18nProvider");
  return ctx;
}

/**
 * Picks the singular or plural source string by count; both are keys of
 * the dictionaries, and {n} is filled with the count.
 */
export function plural(t: Translate, n: number, one: string, other: string, vars?: Vars): string {
  return t(n === 1 ? one : other, { n, ...vars });
}

/** Shortcut for components that only need the translate function. */
export function useT(): Translate {
  return useI18n().t;
}
