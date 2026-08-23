# Migration runbook

This document is a test fixture for media comprehension.

## Reference number

The reference number for this runbook is 52903.

| Database   | File                    | Idempotent |
|------------|-------------------------|------------|
| PostgreSQL | `schema/widget-pg.sql`  | yes        |
| MySQL      | `schema/widget-my.sql`  | yes        |
| SQLite     | `schema/widget-lt.sql`  | no         |

The SQLite variant is **not** idempotent — running it twice creates a
duplicate row. The other two guard with `IF NOT EXISTS`.
