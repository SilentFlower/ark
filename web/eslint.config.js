import js from '@eslint/js'
import pluginVue from 'eslint-plugin-vue'
import tseslint from 'typescript-eslint'

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**'] },
  js.configs.recommended,
  tseslint.configs.recommended,
  pluginVue.configs['flat/recommended'],
  {
    files: ['**/*.vue'],
    languageOptions: {
      parserOptions: {
        parser: tseslint.parser,
      },
    },
  },
  {
    rules: {
      // TypeScript 编译器已经检查未定义标识符，再开一遍只会对 window / document
      // 这类浏览器全局误报。
      'no-undef': 'off',
      // 单词组件名在本项目是刻意的：视图与组件目录已经提供了上下文，
      // 再加前缀只会让模板变长。
      'vue/multi-word-component-names': 'off',
      // 下面两条是纯排版偏好。本项目的 lint 目标是抓真问题，
      // 与后端「gofmt + go vet 够用，不引入 golangci-lint」的取舍保持一致。
      'vue/max-attributes-per-line': 'off',
      'vue/singleline-html-element-content-newline': 'off',
    },
  },
)
