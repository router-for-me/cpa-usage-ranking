import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

export const port = Number(process.env.RANKING_TEST_PORT ?? 4187)
if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('RANKING_TEST_PORT must be 1–65535')
export const outputDir = process.env.PLAYWRIGHT_OUTPUT_DIR ?? fileURLToPath(new URL('./results', import.meta.url))
export const databasePath = resolve(outputDir, '..', 'databases', String(port), 'ranking.db')
export const oauthOrigin = 'https://ranking.example'
