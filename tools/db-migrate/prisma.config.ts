import 'dotenv/config'
import { defineConfig, env } from 'prisma/config'

export default defineConfig({
  schema: 'schema.prisma',
  migrations: {
    path: 'migrations',
    // Delegate to the npm script so the shell evaluates `&&`. Prisma 7 token-
    // splits this string and execs the first program directly, so chained
    // commands here would silently drop everything after `&&` (regenerating
    // the client but never running tsx). `npm run seed` resolves to
    // `prisma generate && tsx seed/seed.ts` inside a real shell.
    seed: 'npm run seed',
  },
  datasource: {
    url: env('DATABASE_URL'),
  },
})
