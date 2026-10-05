-- +goose Up
-- Per-file coding time from a ship's Hackatime capture, resolved against its
-- repository at capture time. status records how the repository accounts for the
-- path: 'head' (on the default branch; path is the repo path and bytes its size
-- there), 'history' (the captured commits touched it but it is gone from the
-- default branch), 'none' (never seen in this repository; path is the tail of the
-- maker's local path, never the full one). Rewritten on every healthy
-- heartbeats-walk capture; a degraded or file-blind capture leaves the previous
-- rows alone, like the rest of the Hackatime-derived columns.
create table if not exists ariw."submissionFileHours" (
  "submissionId" text not null references public."Submission"("id") on delete cascade,
  path text not null,
  seconds double precision not null,
  bytes bigint,
  status text not null,
  primary key ("submissionId", path)
);

-- +goose Down
drop table if exists ariw."submissionFileHours";
