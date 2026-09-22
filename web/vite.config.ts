/// <reference types="vitest/config" />
import { writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'

const outDir = resolve(__dirname, '../internal/web/dist')

// emptyOutDir wipes the tracked dist/.gitkeep that lets `go:embed all:dist`
// compile in a fresh clone without Node. Put it back after every build so the
// tree stays clean and a `git commit -a` can't commit its deletion.
function keepGitkeep(): Plugin {
  return {
    name: 'duskrun-keep-gitkeep',
    apply: 'build',
    closeBundle() {
      writeFileSync(resolve(outDir, '.gitkeep'), '')
    },
  }
}

// Build output goes straight into the Go embed package so `go build` picks up
// the SPA. base:'/' — the app is served from the domain root alongside /api.
export default defineConfig({
  base: '/',
  plugins: [react(), keepGitkeep()],
  build: {
    outDir,
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/setupTests.ts',
    css: false,
  },
})
