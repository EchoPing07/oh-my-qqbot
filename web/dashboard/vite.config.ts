import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'
import path from 'path'

import { dashboardVersionDefine } from './app-version'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  define: dashboardVersionDefine,
  // vitest 复用 vite 配置；node 环境跑纯逻辑单测
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
    globals: false,
  },
  server: {
    host: '127.0.0.1',
    port: 7999,
    proxy: {
      // Go 后端（internal/webui）：REST + WS（/api/webui/ws、/api/webui/ws/logs）
      '/api': {
        target: 'http://127.0.0.1:8001',
        changeOrigin: true,
        ws: true,
      },
    },
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  build: {
    // 每个路由页独立分包，避免单 chunk 过大（首屏只加载 auth/layout/index）
    rollupOptions: {
      output: {
        manualChunks: {
          react: ['react', 'react-dom'],
          router: ['@tanstack/react-router'],
          query: ['@tanstack/react-query'],
        },
      },
    },
  },
})
