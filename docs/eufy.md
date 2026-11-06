# Eufy Life

The native Go client uses the same Eufy Life cloud protocol as these public
projects, implemented with the Go standard library:

- [m4ary/eufylife-api-hacs](https://github.com/m4ary/eufylife-api-hacs)
- [MatthewHallCom/eufylife-mcp](https://github.com/MatthewHallCom/eufylife-mcp)
- [ohsugeek/eufylife-api](https://github.com/ohsugeek/eufylife-api)

This is an unofficial, undocumented API. The implementation follows their login
request, account-token headers, device-data response shapes and measurement units.
It does not run Python, Home Assistant, or an MCP server.

## Credentials

Fill in these existing fields in the ignored, mode-0600 `.secrets/config.env`:

```dotenv
EUFYLIFE_EMAIL=
EUFYLIFE_PASSWORD=
```

Use the direct email/password login for the Eufy Life app. Enter literal values
without surrounding quotes; do not source this file into a shell. If the account
uses Apple or Google sign-in, it may need an Eufy password first. Both fields can
remain blank to disable Eufy; setting only one is a configuration error.

Run `scripts/encrypt-secret.sh .secrets/config.env PATH_TO_CONTAINERNODE`, update
the deployment's `checksum/config` annotation to the encrypted Secret file's
SHA-256, commit and push the encrypted Secret and Deployment, then reconcile Flux.
The deployment loads both variables from `lifeblood-config`. Keep the existing
API token and Oura settings when updating the Secret. No Eufy OAuth registration
or browser authorization is needed. Account credentials are sent only to the
fixed HTTPS origin `api.eufylife.com`; HTTP redirects are refused.

## REST API

Every endpoint requires `Authorization: Bearer <API_TOKEN>` and returns JSON
with `Cache-Control: no-store`. Server-side caching does not permit browser or
proxy caching of health data.

| Endpoint | Response |
| --- | --- |
| `GET /eufy/connection` | Configuration, last connection result, cache freshness, and retry time. Does not authenticate or fetch data. |
| `GET /eufy/members` | `data` array containing `id`, `name`, and `default`. Member metadata comes from the most recent login. |
| `GET /eufy/devices` | `data` array containing `id`, `name`, and optional `alias_name`. Device metadata comes from the most recent login. |
| `GET /eufy/latest-weight?customer_id=ID` | `data` object containing the latest available weight reading for that member. Without an ID, uses the account's default member or the sole known member. Ambiguous accounts return `customer_id_required`. |
| `GET /eufy/measurements?customer_id=ID&start=2026-11-01&end=2026-11-07&limit=100&offset=0` | Newest-first `data` array, matching `total`, `next_offset` (null at the end), and `skipped_records`. All filters are optional. |

History dates are UTC `YYYY-MM-DD`; `start` is inclusive, `end` is exclusive.
No dates means all history returned by Eufy's device-data endpoint, with local
pagination (default 100, maximum 500 per response). This is available cloud
history, not a guarantee that every historical weigh-in exists. Pagination
offsets may shift when the five-minute snapshot refreshes; compare `fetched_at`
when combining pages. Unknown member IDs and absent weight readings return 404.

Measurements expose source IDs, weigh-in `timestamp`, optional `updated_at`,
`weight_kg`, `bmi`, `body_fat_percent`, `body_fat_mass_kg`, `muscle_mass_kg`,
`skeletal_muscle_mass_kg`, `bone_mass_kg`, `water_percent`,
`subcutaneous_fat_percent`, `visceral_fat_level`, `bmr_kcal_per_day`,
`body_age_years`, `protein_percent`, and `heart_rate_bpm`.
Weight is converted from the cloud's tenths of a kilogram. Other mapped metrics
retain the reference clients' units. Missing or zero metrics are null; uncertain
unit mappings are omitted. Readings lacking a member, scale data or a usable
timestamp are counted in `skipped_records`, never given a fabricated timestamp.

## Cache and failures

One successful snapshot is reused for five minutes across every data endpoint,
member, date range and page. Requests arriving together share a single refresh.
The first request after expiry fetches again. Login tokens are reused until near
expiry; HTTP 401/403 permits one re-login, then failure. All network requests and
the complete refresh have timeouts. Response bodies are size-limited.

Errors are also held for five minutes, or longer when Eufy's `Retry-After`
requires it (up to 24 hours), to avoid retry storms and repeated password login
attempts. Expired readings are not returned as a successful fresh response.
The API returns `Retry-After` and a redacted error: 503 for absent/rejected
credentials, 502 for unavailable or malformed upstream responses. Error bodies
and logs exclude account passwords, cloud tokens and cloud response bodies.

Nothing polls Eufy in the background. The cache and login token are per process
and reset on restart; keep a single replica. The API sees data only after the
scale or Eufy Life app has synchronized it to Eufy's cloud. Eufy readings are not
written to PostgreSQL in this version.
