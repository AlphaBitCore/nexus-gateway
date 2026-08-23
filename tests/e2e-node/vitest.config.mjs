import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    include: ['specs/**/*.test.mjs'],
    // Real provider calls; the reasoning specs in particular are slow.
    testTimeout: 120_000,
    hookTimeout: 60_000,
    // One file at a time: the specs hit live upstreams through one virtual key,
    // and parallel files turn a provider rate limit into a spurious failure.
    fileParallelism: false,
    reporters: ['default'],
  },
})
