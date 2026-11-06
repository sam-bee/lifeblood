# Lifeblood

A single-person REST API for Oura and Eufy Life health data. One Go/Echo
application, one repository, one container deployment. There is no browser UI,
SSO, user registration, or Redis.

Production origin: `https://lifeblood.personalinfra.co.uk`.

## API

Every data and control request needs the same secret:

```http
Authorization: Bearer <API_TOKEN>
```

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/oura/connection` | Connection status and collection coverage. |
| GET | `/eufy/connection` | Eufy configuration and cache status; never calls the cloud. |
| GET | `/eufy/members` | Eufy Life member IDs and names. |
| GET | `/eufy/devices` | Eufy Life device IDs and names. |
| GET | `/eufy/latest-weight` | Latest weight and body composition for the default member; optional `customer_id`. |
| GET | `/eufy/measurements` | Available scale history, optional `customer_id`, `start`, `end`, `limit`, `offset`. |
| GET | `/oura/daily` | Sleep, readiness and activity daily source records. |
| GET | `/oura/sleep` | Detailed sleep records, including multiple periods per day. |
| GET | `/oura/collections/{collection}` | One of `daily_sleep`, `daily_readiness`, `daily_activity`, `sleep`. |
| POST | `/oura/sync` | Queue a bounded resync, with JSON `{"start":"YYYY-MM-DD","end":"YYYY-MM-DD"}`. |
| GET | `/oura/sync` | Counts of queued, running, failed and completed jobs. |
| DELETE | `/oura/connection` | Stop collection, cancel work, erase credentials, attempt upstream revocation. |
| DELETE | `/oura/data` | Disconnect and erase live health records, sync metadata, inbox and jobs. |

Oura read endpoints accept `start`, `end`, and `cursor`. End dates are **exclusive**.
Defaults cover 30 days; reads are limited to 90 days and 500 records per page.
Use the returned `next_cursor` with the same dates to read subsequent pages.
Responses preserve Oura payloads and nulls, and include source and coverage
metadata. `fetched_at` means the cloud record was fetched, not that the ring
itself recently synchronized. Coverage describes the latest contiguous range
completed for each collection; it does not claim observations exist on every day.

Eufy Life uses the app's email/password login. Set `EUFYLIFE_EMAIL` and
`EUFYLIFE_PASSWORD` together in `.secrets/config.env`, encrypt and deploy using
the existing SOPS workflow. Blank credentials leave Oura running and Eufy disabled.
Eufy responses share an on-demand **five-minute in-process cache**, including
concurrent requests and local filtering. There is no background polling or cache
bypass. Responses include `cache.fetched_at` and `cache.expires_at`; scale readings
have their own `timestamp`. Eufy data and session tokens stay in memory and are
cleared on restart. See [Eufy setup and API details](docs/eufy.md).

The unauthenticated integration endpoints are `/oura/webhooks` (GET challenge,
POST signed notifications) and `/oura/oauth/callback` (single-use Oura grant).
They expose no existing health records. `/healthz` and `/readyz` are cluster
probes and are excluded from the public ingress. All responses are JSON or empty;
there are no website pages. Full contract: [openapi.yaml](openapi.yaml).

## Configuration and one-time Oura setup

1. Run `python3 scripts/init-secrets.py`. This creates `.secrets/config.env` with
   file mode 0600, generates one API token, an encryption key and a webhook secret,
   and never displays their values. This directory is excluded from Git and Docker.
2. Create an Oura application at <https://developer.ouraring.com/applications>:
   name **Lifeblood**, website (if requested) `https://lifeblood.personalinfra.co.uk`,
   exact redirect **`https://lifeblood.personalinfra.co.uk/oura/oauth/callback`**.
   Initial scopes are `personal daily`.
3. Fill in `OURA_CLIENT_ID`, `OURA_CLIENT_SECRET` and both database URLs in the
   ignored configuration file. See [deployment](docs/deployment.md) for restricted
   database roles and SOPS encryption. Oura credentials may remain blank together
   until registration is complete; data endpoints then report `not_connected`.
4. Deploy the service, then run the operator command:

   ```sh
   kubectl -n lifeblood exec deployment/lifeblood -- /lifeblood link
   ```

   Open the resulting Oura authorization link once within ten minutes. Oura's
   consent screen is hosted by Oura. The callback returns `{"status":"connected"}`.
   This is setup only: the REST API has no login or consent-URL creation endpoint.
   The first linked Oura account is pinned; relinking another account is rejected.

Oura requires this one-time account grant even for your own ring; the developer
client ID/secret do not grant account access. Tokens are refreshed automatically
and the replacement tokens are stored encrypted in PostgreSQL. SOPS stores the
static application credentials, API token and token-encryption key; it is not
rewritten every time Oura rotates a refresh token. An uncertain refresh failure
sets `reconnect_required`; rerun the operator command to relink the same account.

## Ingestion and storage

The worker establishes and renews webhook subscriptions before the initial
90-day history import. Signed notifications are deduplicated and committed to
PostgreSQL before acknowledgement. Durable jobs recover expired leases after a
restart. The worker follows pagination, handles rate limits, keeps deletion
tombstones and reconciles a seven-day overlap daily and after restarts. An
extended outage may require `POST /oura/sync` with a wider range.

Default live-data retention is **365 days**. Jobs and inbox metadata are retained
for 30 days; OAuth transactions expire after ten minutes. Deletion removes live
health payloads and credentials but retains the minimal pinned Oura account ID,
so a different person's account cannot silently replace it. Backups follow the
cluster backup policy; verify its retention and restoration before importing
real data. Keep the token-encryption key with encrypted backups. Key rotation
requires re-encrypting stored tokens, not simply replacing the configuration key.

Rate limits are in-memory: 2 requests/second per client IP (burst 30), 5/second
for authenticated API traffic (burst 30), and two resync requests with a refill
of one per minute. Traefik adds an outer limit. Only explicitly trusted proxy
CIDRs can supply forwarded client IPs. Counters reset on restart; keep one
replica while using these limits. API credentials are accepted only in headers.
Logs omit URLs, query strings, tokens, authorization headers and health payloads.

The ChatGPT/MCP data-path decision from the earlier Oura research is deferred;
this implementation provides REST and does not expose an MCP server.

## Development

Requires Go 1.26.10 or later and a disposable PostgreSQL database:

```sh
TEST_DATABASE_URL='postgres://postgres:local-test-only@localhost:5432/lifeblood_test?sslmode=disable' go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Without `TEST_DATABASE_URL`, PostgreSQL integration tests are skipped. Tests use
synthetic records and a separate temporary schema per test. Never target a
production database with the test environment variable.

Build: `go build -o bin/lifeblood ./cmd/lifeblood`. Commands are `serve`, `migrate`
and `link`. Migration needs only `DATABASE_URL`; serving and linking use the
configuration above. Migration runs separately from the restricted runtime role.

Pushes to `main` run tests, vet and vulnerability checks, then publish private
`ghcr.io/sam-bee/lifeblood:latest` and commit-SHA tags. Flux commits digest updates
to the infrastructure repository, matching the existing Casefile release flow.
GitHub Actions use commit-pinned actions and the repository's `GITHUB_TOKEN`.
