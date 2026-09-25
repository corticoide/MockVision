import { Check, Copy } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";

/**
 * Copies text to the clipboard. The Clipboard API only exists in secure
 * contexts (HTTPS or localhost), and the panel is usually opened over plain
 * HTTP on the LAN, so it falls back to a hidden textarea and execCommand.
 */
export async function copyText(text: string): Promise<boolean> {
  if (window.isSecureContext && navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through to the legacy path
    }
  }
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.className = "fixed -left-[9999px] top-0 opacity-0";
  document.body.appendChild(area);
  const selection = document.getSelection();
  const previous = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null;
  area.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  area.remove();
  if (previous && selection) {
    selection.removeAllRanges();
    selection.addRange(previous);
  }
  return ok;
}

export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(t);
  }, [copied]);

  return (
    <Button
      size="icon"
      variant="ghost"
      title={copied ? "Copied" : label}
      aria-label={label}
      className={copied ? "text-ok hover:text-ok" : undefined}
      onClick={async () => {
        if (await copyText(text)) {
          setCopied(true);
          toast("Copied to the clipboard", "ok");
        } else {
          toast("The browser did not allow copying; select the text and copy it by hand", "error");
        }
      }}
    >
      {copied ? <Check /> : <Copy />}
    </Button>
  );
}
