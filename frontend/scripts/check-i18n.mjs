// Checks that every text the panel translates with a literal key, t("…")
// or plural(t, n, "…", "…"), has its Spanish entry. Keys built at run time
// (labels looked up from a map) are not seen and must be added by hand, as
// must texts left out of t() altogether.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

const root = new URL("../src/", import.meta.url).pathname;
const files = [];
const walk = (dir) => {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p);
    else if (/\.(tsx?|mts)$/.test(name) && !name.endsWith(".d.ts") && name !== "i18n.es.ts") files.push(p);
  }
};
walk(root);

// A literal in double or single quotes.
const lit = String.raw`(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)')`;
const patterns = [
  new RegExp(String.raw`\bt\(\s*` + lit, "g"),
  new RegExp(String.raw`\bplural\(\s*t\s*,[^,]+,\s*` + lit + String.raw`\s*,\s*` + lit, "g"),
];
const unquote = (s, quote) => (quote === "'" ? JSON.parse(`"${s.replace(/\\'/g, "'").replace(/"/g, '\\"')}"`) : JSON.parse(`"${s}"`));
const used = new Map();
for (const f of files) {
  const src = readFileSync(f, "utf8");
  for (const re of patterns) {
    for (const m of src.matchAll(re)) {
      for (let i = 1; i < m.length; i += 2) {
        if (m[i] !== undefined) used.set(unquote(m[i], '"'), f.slice(root.length));
        else if (m[i + 1] !== undefined) used.set(unquote(m[i + 1], "'"), f.slice(root.length));
      }
    }
  }
}

const dict = readFileSync(join(root, "lib/i18n.es.ts"), "utf8");
const known = new Set();
// Keys are quoted, or bare identifiers where the formatter leaves them so.
for (const m of dict.matchAll(new RegExp(String.raw`^\s*(?:` + lit + String.raw`|([A-Za-z_$][\w$]*))\s*:`, "gm"))) {
  known.add(m[1] !== undefined ? unquote(m[1], '"') : m[2] !== undefined ? unquote(m[2], "'") : m[3]);
}

const missing = [...used].filter(([k]) => !known.has(k));
for (const [k, f] of missing) console.error(`missing Spanish text for "${k}" (${f})`);
console.log(`${used.size} texts, ${known.size} Spanish entries, ${missing.length} missing`);
process.exit(missing.length ? 1 : 0);
