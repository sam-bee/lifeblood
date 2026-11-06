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

The existing DNS01 Cloudflare certificate issuer issues the host certificate.
Both `/oura` and `/eufy` are publicly routed to the same authenticated API.
Port 80 redirects to HTTPS. The API remains private behind its bearer token even
though the hostname is publicly reachable. Webhooks and the setup callback use
their own cryptographic checks. Keep ingress access logs disabled or redact
callback and webhook query strings.

NetworkPolicy allows ingress from Traefik, DNS, the shared PostgreSQL 18 service,
and outbound public HTTPS. Standard Kubernetes policies cannot restrict egress
by DNS name; the application uses fixed Oura and Eufy hosts and refuses HTTP redirects.
Verify actual CNI policy enforcement. No Redis, public database port, application
volume or browser authentication provider is required.

## Verification and operation

```sh
kubectl -n lifeblood rollout status deployment/lifeblood
kubectl -n lifeblood get certificate,ingress
flux get images all -A
```

An unauthenticated `GET https://lifeblood.personalinfra.co.uk/oura/connection`
must return 401. With `Authorization: Bearer <API_TOKEN>`, it reports
`not_connected` until one-time Oura linking is completed. A wrong token, including
a token placed in the URL, must be rejected. Confirm HTTP redirects to HTTPS.

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
