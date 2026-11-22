import { createHash, randomBytes } from 'node:crypto'
import { existsSync } from 'node:fs'
import { DatabaseSync } from 'node:sqlite'
import { databasePath } from './test-paths'

const cpaUUID = '550e8400-e29b-41d4-a716-446655440000'

export function withTestDatabase<T>(write: (db: DatabaseSync) => T): T {
  if (!existsSync(databasePath)) throw new Error('Expected the isolated Playwright database')
  const db = new DatabaseSync(databasePath)
  try {
    db.exec('PRAGMA busy_timeout=5000; BEGIN IMMEDIATE')
    const result = write(db)
    db.exec('COMMIT')
    return result
  } finally { db.close() }
}

export function insertTestSeat(db: DatabaseSync, name: string, avatar: number, disabled = false) {
  const insert = db.prepare(`INSERT INTO clients(github_id,cpa_uuid,display_name,avatar_id,disabled,created_at_ms)
    SELECT coalesce(max(github_id),0)+1,?,?,?,?,? FROM clients`)
  insert.setReadBigInts(true)
  return insert.run(cpaUUID, name, avatar, Number(disabled), Date.now()).lastInsertRowid
}

// UI tests submit real reports but do not exercise OAuth. External OAuth is
// covered separately by a fake transport in Go, never by a production bypass.
export function seedReportingSeat(name: string, avatar: number) {
  const raw = randomBytes(32)
  const secret = raw.toString('base64url')
  const hash = createHash('sha256').update(raw).digest()
  const id = withTestDatabase((db) => {
    const id = insertTestSeat(db, name, avatar)
    db.prepare('INSERT INTO client_writers(client_id,token_hash) VALUES(?,?)').run(id, hash)
    return String(id)
  })
  return { id, headers: { Authorization: `Bearer ${secret}` } }
}
