-- +goose Up
-- When a person was last seen eligible while a ship of this project (program +
-- externalId, any version) was on the queue. Eligibility follows the project.
create table if not exists ariw."projectMakerEligibility" (
  "programId" text not null references public."Program"(id) on delete cascade,
  "externalId" text not null,
  "makerId" text not null references public."Maker"(id) on delete cascade,
  "firstSeenAt" timestamptz not null default now(),
  "lastSeenAt" timestamptz not null default now(),
  primary key ("programId", "externalId", "makerId")
);

-- +goose Down
drop table if exists ariw."projectMakerEligibility";
