# ari webhooks

the interesenting side of ari!

what this does is:

- accepts signed ship webhooks from program backends (`POST /api/ingest/:program`, plus withdraw and status),
- does the processing for all the ships sent to ari and converts it in a way the frontend can understand
- delivers signed outbound webhooks to programs, with retries,
- runs periodic jobs (stale review claims, stuck ships, project reprocessing),
- serves the internal endpoints the web app calls (`/internal/reenrich`, `/internal/reenrich-program`, `/internal/filesource`, `/internal/readme`).

## how to run on pc!

you'll need Docker, and the [web repository](https://github.com/hackclub/ari) checked out beside this one for its database and schema.

```sh
# in ../ari-next: local postgres on port 5433, then the shared schema
bun run db:up
bun run db:deploy

# here
cp .env.example .env
# set TOKEN_ENC_KEY in .env: openssl rand -base64 32 (use the web app's key if it already has one)
make image-public
docker run --rm --env-file .env -e DATABASE_URL=postgres://ari:ari@host.docker.internal:5433/ari -p 8080:8080 ari-webhooks:public
curl localhost:8080/healthz
```

you can also run it with go directly with `go run ./cmd/ariwebhooks`. on start the service applies its own migrations (the `ariw` schema, `internal/db/migrate/sql`). to have the web app call this service, set `INTERNAL_API_TOKEN` here and `WEBHOOKS_URL` and `WEBHOOKS_INTERNAL_TOKEN` there.

## configuration

`.env.example` lists every variable if you want to get fancy and get all the env variables! only `DATABASE_URL` and `TOKEN_ENC_KEY` are required.

## ingestion versions

`internal/ingest.CurrentEvidenceVersion` identifies the ingestion version shipped by this service.
new submissions store that private checkpoint in `ariw.submissionEvidenceVersion`, so when a change to
capture or hours math requires existing submissions to be reingested, increment the constant.

this checkpoint is independent of `Submission.ingestVersion`, which is the sender-provided revision
of a ship's webhook payload.