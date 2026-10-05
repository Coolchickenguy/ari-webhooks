-- +goose Up
-- Permanent CDN copies of past-project screenshots, keyed by the source
-- attachment id (stable while the expiring download URL rotates).
-- Written once per attachment during evidence capture; later captures reuse
-- the stored URL instead of uploading another copy.
create table if not exists ariw."cdnScreenshots" (
  "attachmentId" text primary key,
  "cdnUrl" text not null
);

-- +goose Down
drop table if exists ariw."cdnScreenshots";
