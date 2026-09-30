import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

export default defineConfig({
  plugins: [react()],
  server: {
    // Everything under /api is forwarded to the Go server, so the browser only
    // ever talks to one origin. The alternative is CORS headers on the Go side,
    // which would mean changing the server to suit the development setup of a
    // tool that only reads from it.
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
