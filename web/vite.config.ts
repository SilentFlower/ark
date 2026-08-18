import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

// ark-hub 的开发地址。dev server 把 API、登录与登出全部代理到真实 Hub，
// 这样浏览器与 Hub 同源，SameSite=Strict 的会话 Cookie 才会被带上。
const hubOrigin = process.env.ARK_HUB_ORIGIN ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // 刻意输出到 web/dist 而不是直接写进 go:embed 目录。
    // Vite 的 emptyOutDir 会清空整个输出目录，包括那个保证
    // `go:embed dist` 能编译的 PLACEHOLDER 文件——一旦被删，
    // 干净 clone 上没跑过前端构建的 `go build ./...` 就会失败。
    // 同步到 internal/hub/webui/dist 的工作交给 make web-build。
    emptyOutDir: true,
    // 文件名带内容 hash，配合后端的 immutable 缓存头；升级后不会吃到旧 JS。
    assetsDir: 'assets',
  },
  server: {
    proxy: {
      '/api': hubOrigin,
      '/login': hubOrigin,
      '/logout': hubOrigin,
      '/healthz': hubOrigin,
    },
  },
  test: {
    environment: 'happy-dom',
    include: ['src/**/*.spec.ts'],
  },
})
