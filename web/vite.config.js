import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Dev-only proxy. The gateway splits its surface across two ports:
//   :8080  public   -> /v1/*      (chat completions, streaming)
//   :9090  admin    -> /admin/*   (summary, request logs, teams, chaos, ...)
// The frontend calls same-origin relative paths (/v1/..., /admin/...) so no
// CORS handling is needed and no port is baked into the app. In production
// (Phase 10) the built assets are served behind the same origin as these
// paths, so the app code stays identical.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/v1': { target: 'http://localhost:8080', changeOrigin: true },
      '/admin': { target: 'http://localhost:9090', changeOrigin: true },
    },
  },
})
