import path from 'node:path'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

const BACKEND = process.env.BACKEND_URL ?? 'http://localhost:12337'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    proxy: {
      '/api/ws': {
        target: BACKEND.replace(/^http/, 'ws'),
        ws: true,
        changeOrigin: true,
      },
      '/api': {
        target: BACKEND,
        changeOrigin: true,
      },
    },
  },
})
