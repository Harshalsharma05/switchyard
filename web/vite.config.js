import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Dev-only proxy. The gateway splits its surface across two ports:
//   :8080  public   -> /v1/*      (chat completions, streaming)
//   :9090  admin    -> /admin/*   (summary, request logs, teams, chaos, ...)
//   :9090  admin    -> /auth/*    (google sign-in, refresh, logout)
// /auth must come through this origin, not straight to :9090, so the session
// cookies are first-party to the console and the flow matches production.
// The frontend calls same-origin relative paths (/v1/..., /admin/...) so no
// CORS handling is needed and no port is baked into the app. In production
// web/nginx.conf does the same job — serves the built assets and proxies
// /v1 and /admin to the two gateway ports — so the app code stays identical.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/v1': { target: 'http://localhost:8080', changeOrigin: true },
      '/admin': { target: 'http://localhost:9090', changeOrigin: true },
      '/auth': { target: 'http://localhost:9090', changeOrigin: true },
    },
  },
})
