import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The build goes straight into the Go package that embeds it (ADR-012), so
// `make webui && make build` produces one binary with the canvas inside.
export default defineConfig({
  plugins: [react()],
  base: '/ui/',
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    // Keep the placeholder's gitignore alongside the build so a checkout that
    // never ran `make webui` still builds.
    assetsDir: 'assets',
  },
})
