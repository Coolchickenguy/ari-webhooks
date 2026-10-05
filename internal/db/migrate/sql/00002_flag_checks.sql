-- +goose NO TRANSACTION
-- +goose Up
-- FlagKind is declared in ari's Prisma schema, not here. These two values are
-- additive and guarded, so ari can declare them in its own migration later
-- without this one colliding, and re-running is a no-op either way.
alter type "FlagKind" add value if not exists 'NO_README';
alter type "FlagKind" add value if not exists 'PLAGIARISM';

-- One row per source file kept from a ship's default branch. hash is the file's
-- git blob id.
create table if not exists ariw."submissionFile" (
  "submissionId" text not null references public."Submission"("id") on delete cascade,
  path text not null,
  hash text not null,
  bytes bigint not null,
  primary key ("submissionId", path)
);
-- Lookups by hash run across every program.
create index if not exists "submissionFile_hash" on ariw."submissionFile" (hash);

-- +goose Down
drop table if exists ariw."submissionFile";
-- Postgres cannot drop an enum value, so NO_README and PLAGIARISM stay on
-- FlagKind after a rollback. Harmless: nothing writes them once the code is gone.
