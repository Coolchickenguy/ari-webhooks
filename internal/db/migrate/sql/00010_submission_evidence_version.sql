-- +goose Up
-- Tracks the ari-webhooks evidence algorithm independently from the sender's
-- public Submission.ingestVersion payload revision.
create table if not exists ariw."submissionEvidenceVersion" (
  "submissionId" text primary key references public."Submission"(id) on delete cascade,
  version int not null check (version > 0)
);

-- Version 1 predates this private checkpoint table. Recording it explicitly
-- keeps deployment inert until the evidence algorithm is intentionally bumped.
insert into ariw."submissionEvidenceVersion" ("submissionId", version)
select id, 1 from public."Submission"
on conflict ("submissionId") do nothing;

-- +goose Down
drop table if exists ariw."submissionEvidenceVersion";
