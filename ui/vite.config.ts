import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies the API to a running lowpathd on :7878.
//
// The build goes to dist/app, which git ignores; emptyOutDir clears only
// that folder, so the tracked dist/placeholder (what a Go build without the
// UI shows, see embed.go) is never touched.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: { "/api": { target: "http://127.0.0.1:7878", changeOrigin: true } },
  },
  build: { outDir: "dist/app", emptyOutDir: true, chunkSizeWarningLimit: 800 },
});
