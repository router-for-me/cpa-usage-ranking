import { defineConfig } from '@playwright/test'
import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'
import { databasePath, oauthOrigin, outputDir, port } from './test-paths'

const baseURL = `http://127.0.0.1:${port}`
const binary = resolve(process.env.RANKING_TEST_BINARY ?? fileURLToPath(new URL('../../bin/ranking', import.meta.url)))
const webDir = resolve(process.env.RANKING_TEST_WEB_DIR ?? fileURLToPath(new URL('../dist', import.meta.url)))
const adminConfig = process.env.RANKING_TEST_ADMIN_CONFIG
const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined
const channel = process.env.PLAYWRIGHT_CHROMIUM_CHANNEL || undefined
if (executablePath && channel) throw new Error('Choose PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH or PLAYWRIGHT_CHROMIUM_CHANNEL, not both')

export default defineConfig({
  testDir: '.',
  testMatch: '**/*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  outputDir,
  reporter: 'list',
  use: {
    baseURL,
    browserName: 'chromium',
    channel,
    launchOptions: executablePath ? { executablePath } : undefined,
    viewport: { width: 1440, height: 1000 },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  webServer: {
    command: JSON.stringify(binary),
    url: `${baseURL}/healthz`,
    cwd: fileURLToPath(new URL('../..', import.meta.url)),
    env: {
      RANKING_HTTP_ADDR: `127.0.0.1:${port}`,
      RANKING_WEB_DIR: webDir,
      ...(adminConfig ? { RANKING_ADMIN_CONFIG_FILE: resolve(adminConfig), RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP: 'true' } : {}),
      RANKING_MODELS_PATH: fileURLToPath(new URL('../../config/models.json', import.meta.url)),
      RANKING_SHUTDOWN_TIMEOUT: '5s',
      RANKING_CACHE_REFRESH_INTERVAL: adminConfig ? '5m' : '200ms',
      RANKING_SQLITE_PATH: databasePath,
      RANKING_REGISTRATION_ENABLED: 'true',
      RANKING_PUBLIC_ORIGIN: oauthOrigin,
      RANKING_GITHUB_CLIENT_ID: 'playwright-fixture',
      RANKING_GITHUB_CLIENT_SECRET: 'playwright-fixture-not-a-secret',
    },
    reuseExistingServer: false,
    timeout: 15_000,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 8_000 },
  },
})
