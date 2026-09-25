import { Languages } from "lucide-react";
import { type Locale, locales, useI18n } from "@/lib/i18n";
import { cn } from "@/lib/utils";

/** Compact language picker. The choice is saved per browser. */
export function LanguageSelect({ className }: { className?: string }) {
  const { locale, setLocale, t } = useI18n();
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-muted [&_svg]:size-3.5", className)} title={t("Language")}>
      <Languages aria-hidden />
      <select
        value={locale}
        onChange={(e) => setLocale(e.target.value as Locale)}
        aria-label={t("Language")}
        className="cursor-pointer rounded-sm bg-transparent py-0.5 text-[13px] text-text focus:outline-none"
      >
        {locales.map((l) => (
          <option key={l.id} value={l.id}>
            {l.name}
          </option>
        ))}
      </select>
    </span>
  );
}
