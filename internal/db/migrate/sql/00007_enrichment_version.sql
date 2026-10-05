-- +goose Up
-- Submission belongs to ari's Prisma schema; guarded so ari can declare the
-- column in its own migration without this one colliding. Incremented inside
-- every evidence-capture persist (first enrich and each re-enrich), so ari can
-- tell one snapshot from the next; 0 means never captured.
alter table public."Submission"
  add column if not exists "enrichmentVersion" int not null default 0;

-- +goose Down
alter table public."Submission" drop column if exists "enrichmentVersion";
