-- +goose Up
-- The maker's (and collaborators') prior projects, read from an external
-- ledger during evidence capture, one row per ledger
-- record. Rewritten on every capture whose ledger read succeeded (first enrich
-- and each re-enrich); a degraded or unconfigured read leaves the previous
-- rows alone, like the rest of the captured evidence. approvedAt is null for
-- ledger rows not yet stamped with an approval date.
create table if not exists ariw."submissionPastProjects" (
  "submissionId" text not null references public."Submission"("id") on delete cascade,
  "recordId" text not null,
  "recordUrl" text not null,
  email text not null,
  programs text[] not null,
  "codeUrl" text not null,
  "playableUrl" text not null,
  description text not null,
  "screenshotUrl" text not null,
  "hackatimeProjects" text[] not null,
  "creditedHours" double precision not null,
  "approvedAt" timestamptz,
  primary key ("submissionId", "recordId")
);

-- +goose Down
drop table if exists ariw."submissionPastProjects";
