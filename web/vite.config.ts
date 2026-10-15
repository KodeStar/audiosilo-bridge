/// <reference types="vitest/config" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { fileURLToPath, URL } from 'node:url';

// Dev loop:
//   1. Run the Bridge on 127.0.0.1:8091 (`./bin/audiosilo-bridge serve`; its first start
//      prints the page's password in the log).
//   2. In this directory run `npm run dev` (Vite serves the page on http://localhost:5173).
//   3. The dev server proxies `/api` to the Bridge, so the page talks to the real backend
//      same-origin during development. The Bridge serves its own embedded build
//      same-origin in production, so no proxy is needed there.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // Emitted to web/dist; scripts/build-web.sh copies it into internal/web/dist.
    outDir: 'dist',
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8091',
        changeOrigin: true,
      },
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: true,
  },
});
