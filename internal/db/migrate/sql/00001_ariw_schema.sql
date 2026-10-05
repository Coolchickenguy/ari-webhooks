-- +goose Up
create schema if not exists ariw;

create table ariw.job (
  id bigint generated always as identity primary key,
  kind text not null,
  "submissionId" text not null,
  attempt int not null default 0,
  "timeoutRetries" int not null default 0,
  "runAt" timestamptz not null,
  status text not null default 'due',
  "claimedBy" text,
  "claimedAt" timestamptz,
  "lastError" text,
  "createdAt" timestamptz not null default now(),
  "finishedAt" timestamptz
);
create unique index "job_live_per_submission" on ariw.job (kind, "submissionId") where status in ('due', 'running');
create index "job_due" on ariw.job ("runAt") where status = 'due';

create table ariw."outboundSchedule" (
  "deliveryId" text primary key references public."OutboundDelivery"("id") on delete cascade,
  "nextAttemptAt" timestamptz not null,
  "leaseUntil" timestamptz not null default '-infinity',
  "workerId" text
);
create index "outboundSchedule_due" on ariw."outboundSchedule" ("nextAttemptAt");

-- +goose Down
drop schema ariw cascade;
