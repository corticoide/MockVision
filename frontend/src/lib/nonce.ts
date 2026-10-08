import { setNonce } from "get-nonce";

/**
 * The nonce of this page's styles: the server stamps a new one into
 * index.html on every response and allows only the style elements that
 * carry it (D49). Components that add a style element at run time, such as
 * the scroll lock of a modal dialog, take it from here.
 */
function readNonce(): string | undefined {
  const v = document.querySelector<HTMLMetaElement>('meta[name="csp-nonce"]')?.content;
  // The development server serves the page unstamped.
  return v && !v.startsWith("__") ? v : undefined;
}

export const cspNonce = readNonce();
if (cspNonce) setNonce(cspNonce);
