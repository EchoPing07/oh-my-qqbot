import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig, type Plugin } from 'vite'
import path from 'path'

import { dashboardVersionDefine } from './app-version'

// chokidar 对 EBUSY 等错误会 emit 'error'，而 Vite dev server 未监听该事件，
// 单个文件监听失败（Node 的 'error' 无监听即抛出）会直接打挂 dev 服务。
function watchErrorGuard(): Plugin {
  return {
    name: 'watch-error-guard',
    configureServer(server) {
      server.watcher.on('error', (error: Error) => {
        server.config.logger.warn(`文件监听出错，已忽略: ${error.message}`)
      })
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [tailwindcss(), react(), watchErrorGuard()],
  define: dashboardVersionDefine,
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
    // CodeMirror 扩展依赖同一份 state/view 单例；混用包管理器时重复实例会导致扩展解析失败。
    dedupe: ['@codemirror/state', '@codemirror/view'],
  },
  optimizeDeps: {
    // 设置页及其懒加载编辑器在启动时预构建，避免首次访问时重新优化依赖，
    // 使正在加载的模块请求因依赖版本变化返回 504 Outdated Optimize Dep。
    include: [
      'react',
      'react-dom',
      '@radix-ui/react-accordion',
      '@codemirror/lang-css',
      '@codemirror/lang-json',
      '@codemirror/lang-python',
      '@codemirror/language',
      '@codemirror/legacy-modes/mode/toml',
      '@codemirror/lint',
      '@codemirror/state',
      '@codemirror/theme-one-dark',
      '@codemirror/view',
      '@uiw/react-codemirror',
    ],
  },
  build: {
    // 让 Rollup 按实际依赖关系分包，避免手动拆分的 Router/Radix 包互相导入，
    // 导致生产版在 React 初始化前访问 forwardRef 而白屏。
    chunkSizeWarningLimit: 500,
  },
})
