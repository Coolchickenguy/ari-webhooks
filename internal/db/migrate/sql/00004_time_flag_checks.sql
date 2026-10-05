-- +goose NO TRANSACTION
-- +goose Up
-- Guarded and additive for the same reason as 00002: FlagKind belongs to ari's
-- Prisma schema, and ari may declare these values there later.
alter type "FlagKind" add value if not exists 'MARATHON_SESSION';
alter type "FlagKind" add value if not exists 'HOURS_REUSE';
alter type "FlagKind" add value if not exists 'IDLE_DEVLOG';

-- +goose Down
-- Postgres cannot drop enum values, so these stay on FlagKind after a rollback.
-- Harmless: nothing writes them once the code is gone.
select 1;
