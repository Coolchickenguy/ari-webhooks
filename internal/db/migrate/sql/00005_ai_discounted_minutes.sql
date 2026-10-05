-- +goose Up
-- HoursBreakdown belongs to ari's Prisma schema; guarded so ari can declare the
-- column in its own migration later without this one colliding.
alter table public."HoursBreakdown"
  add column if not exists "aiDiscountedMinutes" int not null default 0;

-- +goose Down
alter table public."HoursBreakdown" drop column if exists "aiDiscountedMinutes";
