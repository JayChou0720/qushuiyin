import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  base: '/admin/',
  plugins: [vue()],
  build: { outDir: '../../internal/admin/dist', emptyOutDir: true },
  server: { proxy: { '/admin/v1': { target: process.env.ADMIN_API_TARGET || 'http://127.0.0.1:8080', changeOrigin: true } } },
})
