-- +goose Up
-- time is stored in whole seconds: file time becomes an integer (halves round
-- up) and past projects gain a seconds twin beside the float creditedHours,
-- which stays and keeps being written for the old app.
alter table ariw."submissionFileHours"
  alter column seconds type integer using floor(seconds + 0.5)::integer;

alter table ariw."submissionPastProjects"
  add column if not exists "creditedSeconds" integer not null default 0;

update ariw."submissionPastProjects"
set "creditedSeconds" = least(greatest(floor("creditedHours" * 3600 + 0.5), 0), 2147483647)::integer;

-- evidence version 2 captures true seconds. decided ships keep their settled
-- minutes (already backfilled as minutes * 60), so only open ships are recaptured.
insert into ariw."submissionEvidenceVersion" ("submissionId", version)
select id, 2 from public."Submission"
where status in ('approved', 'changes', 'rejected', 'reverted', 'withdrawn')
on conflict ("submissionId") do update set version = 2
where ariw."submissionEvidenceVersion".version < 2;

-- +goose Down
alter table ariw."submissionPastProjects" drop column if exists "creditedSeconds";
alter table ariw."submissionFileHours"
  alter column seconds type double precision;
