# Repository agent instructions

## Code quality

Use **camelCase** (e.g. `thisThingLikeThis`) for naming by default. Exception: when the file being edited uses
a different convention or the language/format forbids it. In that case match what the
file already does — consistency within a file beats the default.

Constants are **not** exempt — do not use `SCREAMING_SNAKE_CASE` for them. A constant
is still a name and follows camelCase like everything else. `const maxAgeSeconds = 300`
not `const MAX_AGE_SECONDS = 300`.

Only declare a `const` (or any variable) if the value is reused in the same file more
than once. If it appears exactly once, inline the value at the call site and add a
trailing comment explaining what it is.

```ts
// bad — single-use variable
const maxAgeSeconds = 300;
if (age > maxAgeSeconds) { … }

// good — inlined with explanation
if (age > 300) { … } // 5-minute window before webhook is considered stale
```

## Imports

Group imports into three blocks, each separated by one blank line: npm modules first, then local imports, then type-only imports. Within each block, sort by line length shortest first.

## Comments

Default: no comments. Names and structure carry meaning; a comment that restates what the code does adds noise.

Never use section divider comments (`// --- Section ---`, `// ===`, `// *** Heading ***`, or any variant). Structure code with whitespace and named functions instead.

The one exception: code whose removal would silently change behavior beyond its immediate scope — security checks, access control, policy scoping, session or cookie manipulation, authorization guards. These **must** have a trailing inline comment explaining why the line exists, not what it does.

If a reader would not know why a line is there without broader context, comment it. Otherwise don't.

## User-facing strings

Any string that will be displayed to a user (labels, messages, errors, tooltips, button text, notifications) must use plain, non-technical wording. Write as if speaking to someone unfamiliar with the codebase or the underlying systems. Do not use em dashes (--) in these strings; use a comma, period, or rephrase instead.

## Open source first

This repository is public. Sensitive code lives in `private/webhooks/`, compiled only with the `private` build tag. `private/` is a checkout of the private repository https://github.com/hackclub/ari-private, which this service shares with the web app: its code sits beside this one, in `private/web/`.

- The repository must build, vet, pass its tests and run with no `private/` checkout and no production environment variables. `make test-public` proves it.
- Sensitive, in the Hack Club sense: fraud review, flags and how they are detected, screening rules, AI checks and their prompts, and any integration with internal tools. Public code reaches these only through the interfaces in `internal/ext`.
- Never commit internal hostnames, Slack channel or user ids, staff email addresses, thresholds, prompts or flag descriptions to this repository, in code, comments, tests, test data or migrations. Configuration defaults are empty.
- Every optional integration degrades gracefully when its environment variable is blank. Update `.env.example` when configuration changes.
- Every Go file under `private/webhooks/` starts with `//go:build private`, and nothing outside `private/` imports it.
- Run Go tooling on named directories (`./cmd/... ./internal/... ./private/webhooks/...`), never `./...`: `private/web/` is not Go, and a `node_modules` folder there can ship `.go` files that `./...` would pick up.
- Migrations are shared with production and already applied: never edit a statement in an existing migration.

## No scratch files in the repository

Do not create scripts, probes, fixtures, logs or any other temporary file inside this repository (or inside `private/`), not even briefly. If you need a script to run something, write it in a temporary folder outside the repository and run it from there. The only files you add are ones that belong to the project and are meant to stay.

## Test data is generic

Fixture and test data must not imitate real people or real projects. Use plainly generic values such as `user1@example.com`, `maker3`, `project12`.

## Time is integer seconds

Store, compute and pass time as integer seconds. Rounding and proportional splits go through `internal/secs`, which round once and preserve totals.

## Integration contract documentation

This service terminates the public wire contracts: inbound ingest webhooks from program backends (`internal/ingest`, `internal/httpapi/ingest.go`) and outbound deliveries to program backends (`internal/outbound`). The documentation integrators build against lives in the web repository under `src/routes/docs/` (served at `/docs` on the Ari frontend), with field tables and example payloads meant to be copied.

Whenever a change alters what an external sender must send or what this service delivers on the wire (payload fields, types, required/optional status, validation rules, accepted values, event names, headers, the signature scheme, response status codes, error bodies, or retry semantics), the `/docs` pages in the web repository must be updated to match. Keep the documented examples exactly in sync with what the code accepts and emits, so an integrator can replicate a request or verify a delivery straight from the docs.

Never ship a contract change silently. If the doc update cannot land together with the change, say so explicitly in the handoff and treat the work as unfinished until the docs match.
