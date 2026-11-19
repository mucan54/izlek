# Deploying izlek on Coolify

[`docker-compose.coolify.yml`](../../docker-compose.coolify.yml) in the repository root runs the whole platform as one Coolify resource on a single server: ingestion, shipper, compactor, query service, control plane with the UI, and a one-off bootstrap step that creates the first user.

> **How this was verified.** CI builds this exact file and runs it with plain Docker Compose (`deploy/compose/smoke.sh coolify`): the stack bootstraps itself, every container becomes healthy, an event is ingested and found by a query, the generated admin password signs in, and a second deploy changes nothing. **It has not been deployed on a real Coolify instance yet**, so the Coolify-specific parts — domain routing through the `SERVICE_URL_*` variables and the `exclude_from_hc` key — follow Coolify's documentation but are untested. Please report what you find.

## What you need

- Coolify v4 with a server that has Docker and the Coolify proxy.
- **Build resources.** The Rust services are compiled on your server during the first deploy. On a 4-vCPU / 16 GB CI runner the whole build takes about 25 minutes. Peak memory was not measured; expect several gigabytes. If the build is killed for lack of memory, set `CARGO_BUILD_JOBS` to `1` or `2` (slower, but lighter). Later deploys reuse Docker's layer cache unless the `engine/` directory changed.
- **Run-time resources.** Idle, the five processes use little memory; the query service may use up to `LA_QUERY_MEMORY_BYTES` (1 GiB here) per running query.
- Two hostnames, or Coolify's generated ones: one for the UI, one for event ingestion.

## Steps

1. **Create the resource.** In your Coolify project choose *New Resource* → your Git source → repository `mucan54/izlek` (or your fork), branch `main`.
2. **Build pack.** Select **Docker Compose**. Set *Base Directory* to `/` and *Docker Compose Location* to `/docker-compose.coolify.yml`.
3. **Environment variables.** Coolify lists the variables the file uses. Only one is required:

   | Variable | Required | Meaning |
   |---|---|---|
   | `ADMIN_EMAIL` | yes | E-mail address of the first (owner) account |
   | `ORG_NAME`, `PROJECT_NAME` | no | Names of the first organization and project |
   | `LA_TRUSTED_PROXIES` | no | Proxies in front of ingestion. `1` (default) = Coolify's proxy; `2` if a CDN such as Cloudflare sits in front of it |
   | `LA_STORE_URL`, `LA_S3_ENDPOINT`, `LA_S3_REGION`, `LA_S3_ACCESS_KEY`, `LA_S3_SECRET_KEY` | no | Use S3-compatible storage instead of the server's disk (see below) |
   | `CARGO_BUILD_JOBS` | no | Limit build parallelism on small servers |
   | `LA_QUERY_MEMORY_BYTES`, `LA_QUERY_CACHE_BYTES`, `LA_SHIPPER_BATCH_MB` | no | Memory and cache budgets |

   Coolify generates three values by itself; you do not set them: `SERVICE_PASSWORD_ADMIN` (the first account's password), `SERVICE_PASSWORD_64_QUERYSECRET` (shared secret between the UI and the query service) and the two `SERVICE_URL_*` addresses.
4. **Domains.** Coolify assigns generated domains to the `control` and `ingest` services. To use your own, open the resource's domain settings and enter, for example, `https://analytics.example.com:8090` for **control** and `https://in.example.com:8080` for **ingest**. The port suffix tells the proxy which container port receives the traffic; visitors still use plain `https://`.
5. **Deploy.** The first deploy is the long one (see above).
6. **Sign in.** Open the control domain and sign in with `ADMIN_EMAIL` and the value of `SERVICE_PASSWORD_ADMIN` from the *Environment Variables* tab. Change the password under *Account*; the generated value is only used when the account is first created. The UI language (English or Turkish) can be switched on the sign-in screen and under *Account*.
7. **Connect your app.** *Project settings → SDK setup* shows the snippets. The SDK `host` is the **ingest** domain; the write token (`pk_…`) is listed under *Write tokens*.

   ```html
   <script src="izlek.min.js"></script>
   <script>
     izlek.init({ token: 'pk_…', host: 'https://in.example.com' })
   </script>
   ```

### The bootstrap step

The `bootstrap` container runs before the control plane on every deploy. On the first one it creates the account, organization, project, a write token and a secret key, and prints the token and the key **once** in its log (`token=pk_…`, `secret=sk_…`). On later deploys it finds everything in place and changes nothing.

Because that secret key appeared in a deployment log, treat it as disposable: create the keys you actually use under *Project settings → Secret keys* and delete the first one.

## Storage and backups

By default everything lives in the `data` volume on the server: events (`/data/store`), the ingestion write-ahead log, the query cache and the control plane's SQLite database. Back this volume up; Coolify's persistent storage is not a backup.

To keep the data in an S3-compatible bucket instead, set:

```
LA_STORE_URL=s3://your-bucket/izlek
LA_S3_ENDPOINT=https://s3.example.com      # omit for AWS
LA_S3_REGION=us-east-1
LA_S3_ACCESS_KEY=…
LA_S3_SECRET_KEY=…
```

The bucket must support conditional writes (`PUT` with `If-None-Match: *`). MinIO is tested in CI; for any other provider run the contract tests in the main README first. With S3 the volume still holds the write-ahead log (acknowledged events that are not uploaded yet) and the admin database, so it must stay persistent.

## Updating

Redeploy from Coolify. Ingestion needs time to seal and upload its write-ahead log on shutdown (`stop_grace_period: 90s`); if the proxy cuts it short, nothing is lost — whatever was not uploaded stays in the volume and is uploaded when the new container starts. With a single ingestion node, SDKs see a short interruption during the swap and retry on their own.

## What this setup does not give you

- More than one server. For several ingestion or query nodes, run the images yourself with a shared `LA_STORE_URL=s3://…` (see the main README).
- Metrics scraping. The processes expose Prometheus metrics on internal ports (`ingest:9100`, `control:9101`, `shipper:9102`, `compactor:9103`, `query:8081`); add a Prometheus container to the stack if you want the alert rules in `deploy/prometheus/alerts.yml`.
- A GeoIP database. Without one, the country of every event is unknown. Mount a country MMDB file into the `ingest` service and set `LA_GEO_DB` to its path.
