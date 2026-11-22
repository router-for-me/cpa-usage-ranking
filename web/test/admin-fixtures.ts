import { insertTestSeat, withTestDatabase } from './database-fixtures'

// Only the isolated SQLite file is seeded; admin queries remain real HTTP.
export function seedUsers(prefix: string, count: number) {
  return withTestDatabase((db) => {
    const users: { id: string; name: string }[] = []
    for (let index = 0; index < count; index++) {
      const name = `${prefix}-${index}${index === 0 ? ' <img>' : ''}`
      users.push({ id: String(insertTestSeat(db, name, 1, index === 0)), name })
    }
    return users
  })
}
