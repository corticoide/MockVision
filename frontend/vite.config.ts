import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The dev server proxies the API and the WebSocket to a running node, for
// example `mockvision serve --net local` on port 8080.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    proxy: {
      "/api": { target: process.env.MOCKVISION_URL ?? "http://127.0.0.1:8080", ws: true },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Everything ships as files: the panel runs under a strict CSP and never
    // loads anything from a CDN.
    assetsInlineLimit: 0,
  },
});
