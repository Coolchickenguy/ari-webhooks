-- +goose NO TRANSACTION
-- +goose Up
-- Guarded and additive for the same reason as 00002: FlagKind belongs to ari's
-- Prisma schema, and ari may declare this value there later.
alter type "FlagKind" add value if not exists 'PHANTOM_FILES';

-- +goose Down
-- Postgres cannot drop an enum value, so PHANTOM_FILES stays on FlagKind after a
-- rollback. Harmless: nothing writes it once the code is gone.
select 1;
