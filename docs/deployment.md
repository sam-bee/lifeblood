# k3s deployment

Use the existing `containernode` infrastructure repository and Flux; it owns the
live manifests. `deploy/kubernetes` is the initial installation template. Once
installed, make cluster-specific changes in `infrastructure/containernode/lifeblood`.

## Database

Use `postgres18.postgres.svc.cluster.local:5432`, alongside Casefile, with a
dedicated `lifeblood` database and separate runtime and migration roles. Use
private generated passwords, never passwords in committed SQL or terminal logs.

With `psql` variables `migration_password` and `runtime_password` set securely:

```sql
CREATE ROLE lifeblood_migrate LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
  PASSWORD :'migration_password';
CREATE ROLE lifeblood_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
  PASSWORD :'runtime_password';
CREATE DATABASE lifeblood OWNER lifeblood_migrate;
REVOKE ALL ON DATABASE lifeblood FROM PUBLIC;
GRANT CONNECT ON DATABASE lifeblood TO lifeblood_app;
\connect lifeblood
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER SCHEMA public OWNER TO lifeblood_migrate;
GRANT USAGE ON SCHEMA public TO lifeblood_app;
ALTER DEFAULT PRIVILEGES FOR ROLE lifeblood_migrate IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO lifeblood_app;
ALTER DEFAULT PRIVILEGES FOR ROLE lifeblood_migrate IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO lifeblood_app;
```

Set `DATABASE_URL` to the runtime login and `MIGRATION_DATABASE_URL` to the
migration login. Use authenticated TLS (`sslmode=verify-full` plus the cluster's
CA trust where provisioned); the existing cluster uses `sslmode=require` for
Casefile, which encrypts traffic but does not authenticate the server identity.
The migration URL is injected only into the init container. The running API
receives only its restricted database credentials.

## Manifests and secrets

1. Fetch and fast-forward the clean infrastructure checkout before editing;
   Flux commits image updates to its `main` branch.
2. Copy `deploy/kubernetes` to `infrastructure/containernode/lifeblood` and add
   `lifeblood` to the parent Kustomization. DNS A record is `51.75.167.127`.
3. Add the generated application Secret, encrypted using the infrastructure
   repository's existing SOPS/age recipient:

   ```sh
   sh scripts/encrypt-secret.sh .secrets/config.env /path/to/containernode
   ```

   The script emits only an encrypted manifest. Add
   `lifeblood-config.secret.sops.yaml` to that Kustomization. The plaintext local
   file stays ignored and mode 0600; remove it once stored in your password manager
   or other secure operator storage.
4. Provision `lifeblood-ghcr` pull Secrets for namespaces `lifeblood` and
   `flux-system`, both SOPS-encrypted. Registry credentials must have
   `read:packages` access to `ghcr.io/sam-bee/lifeblood`. Existing Casefile registry
   credentials may be reused only if their permissions cover the new package;
   copying their values must happen through a secure decrypt/re-encrypt pipeline.
5. The image automation reuses `casefile-flux-write`, the existing GitRepository
   with write access to the infrastructure repository. No new write key is needed.
6. Commit and push manifests. Flux should resolve the first published GHCR digest
   before starting the Deployment. Both init and runtime images have Flux setters.
   Do not imperatively overwrite Flux-managed images.

The existing DNS01 Cloudflare certificate issuer issues the public host certificate.
Traefik routes only the exact paths `/oura/webhooks` and `/oura/oauth/callback`
to the public listener on port 8080. HTTP redirects to HTTPS for these integration
paths only. Retrieval and control paths are absent from the public application
router, including requests with a valid bearer token. Keep ingress access logs
disabled or redact callback and webhook query strings.

The same container runs a second HTTP listener on port 8082. It registers only the
bearer-protected Oura and Eufy retrieval/control routes. Its required
`PRIVATE_BASE_URL` is the full HTTPS origin of your `svc:lifeblood` Tailscale Service;
store it in the SOPS-encrypted `lifeblood-config` Secret, without committing the
actual tailnet name in plaintext. Leave `PUBLIC_BASE_URL` on the public origin so
webhook subscriptions and OAuth redirects remain unchanged.

There are two Kubernetes ClusterIP Services selecting the same pod:
`lifeblood` targets the public listener on port 8080; `lifeblood-api` targets the
private listener on port 8082. The namespace keeps restricted Pod Security,
which forbids hostPort mappings. Neither Service uses a NodePort or a public
LoadBalancer, and the private Service has no public ingress.

Host-level Tailscale Serve proxies directly to the private Service's ClusterIP:

```sh
kubectl -n lifeblood get service lifeblood-api -o jsonpath='{.spec.clusterIP}'
# Use that address in tailscale/run-tailscale-serve.sh:
tailscale serve --service=svc:lifeblood --https=443 http://PRIVATE_CLUSTER_IP:80
```

The infrastructure manifest pins the allocated private ClusterIP so the host's
upstream is stable. When installing from the templates, allocate the private
Service first, record its assigned ClusterIP in the infrastructure manifest, and
use it in the host-level Serve mapping. Preserve this allocation on upgrades.
Do not use the pod IP, which changes on restarts. Modern Linux Tailscale Serve
supports proxying to remote upstreams, including node-reachable ClusterIPs.

Define `svc:lifeblood` with endpoint `tcp:443`, approve its host advertisement if
required, and grant access only to the intended identities in the tailnet policy.
Tailscale terminates private HTTPS; traffic from the host to the cluster uses HTTP.
Probes are absent from the private router. Both listeners share one worker,
connection pool, Eufy cache, container image and release. Both ports must bind
successfully before either listener or ingestion starts; either server failing
stops the process.

NetworkPolicy permits Traefik ingress only to port 8080, plus DNS, the shared
PostgreSQL 18 service and outbound public HTTPS egress. Node-local ingress from
Tailscale Serve is allowed by Kubernetes network-policy semantics; there is no
general pod ingress rule for the private port. Standard Kubernetes policies
cannot restrict egress by DNS name; the application uses fixed Oura and Eufy
hosts and refuses HTTP redirects. Verify actual CNI policy enforcement. No Redis,
public database port, application volume or browser authentication provider is
required.

## Verification and operation

```sh
kubectl -n lifeblood rollout status deployment/lifeblood
kubectl -n lifeblood get certificate,ingress
flux get images all -A
```

A public `GET https://lifeblood.personalinfra.co.uk/oura/connection` must return
404, including with a valid API token. Check all data/control paths and confirm
that `/healthz` and `/readyz` are not public. Confirm that `/oura/webhooks` rejects
an invalid challenge or unsigned notification and that the OAuth callback still
validates its single-use state. Confirm HTTP redirects to HTTPS for integration
paths and that prefix lookalikes are not routed.

From an authorised tailnet client, query `$PRIVATE_BASE_URL/oura/connection`:
without a token it must return 401; with `Authorization: Bearer <API_TOKEN>` it
reports connection/coverage status (`not_connected` before initial linking).
The private address must return 404 for webhook, OAuth and probe paths. A wrong
token, including a token placed in the URL, must be rejected. Verify that port
8082 is unavailable on the node's public address, and that Traefik cannot connect
to the private pod port. Check the private ClusterIP from the node with the private
hostname in the Host header if DNS or Tailscale Service propagation is still pending.

Run `/lifeblood link` through `kubectl exec` as described in the main README.
This command is available only to the cluster operator; it is not a public API.
Confirm daily data, webhook updates, restart recovery and deletion with your
authorized Oura account after synthetic tests pass. Real callback signature
conformance and actual granted scopes cannot be verified with synthetic tests.

To change static credentials, edit the encrypted Secret with `sops`, commit it,
and restart the Deployment once Flux has applied it; environment variables are
loaded on process startup. API token rotation invalidates the previous token.
Back up and restore the dedicated database and token-encryption key. The service
does not restart just because Oura is unavailable; inspect `/oura/connection`,
`/oura/sync` and payload-free worker logs for ingestion failures.

## Request logs in Grafana

Lifeblood writes one JSON `request completed` event to stdout per request, except
successful `/healthz` and `/readyz` probes. Alloy already tails all pod logs and
labels this Deployment with `namespace="lifeblood"`, `app="lifeblood"` and
`container="lifeblood"`, then forwards them to Loki. No sidecar, application Loki
credentials, extra egress rule or collector configuration is required.

In Grafana Explore, select the **Loki** datasource and use:

```logql
{namespace="lifeblood", container="lifeblood"} | json | msg="request completed"
```

Append `| status >= 400` for failures, `| status >= 500` for server errors, or
`| duration_ms > 1000` for requests taking over a second. Access records contain
`method`, `route`, `status`, `duration_ms` and `response_bytes`, plus the JSON
logger's `time`, `level` and `msg`. Successes use INFO, client errors WARN, and
server errors ERROR. Errors and recovered panics use the final response status.
Only registered route templates are logged; unmatched paths become
`<unmatched>`. Request/response bodies, raw URLs, query strings, route parameter
values, headers, client IPs and raw errors are excluded.

The existing cluster Loki configuration sets both `retention_period` and
`max_query_lookback` to **168h (seven days)**. Its compactor runs every ten minutes
with retention enabled and a two-hour `retention_delete_delay`; physical deletion
is asynchronous after chunks are marked. This is a rolling log window, separate
from Lifeblood's 365-day health-record retention in PostgreSQL. The infrastructure
repository's `observability/loki-helmrelease.yaml` owns these settings.
