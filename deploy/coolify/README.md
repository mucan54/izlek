# Deploying izlek on Coolify

This guide takes you from an empty Coolify project to a working analytics installation on your own domain: the dashboard at `https://analytics.example.com`, event collection at `https://in.analytics.example.com`, and optionally Claude or ChatGPT connected to it.

Everything runs as **one Coolify resource** built from [`docker-compose.coolify.yml`](../../docker-compose.coolify.yml) in the repository root.

> **Tested so far.** CI builds and runs this exact file on every push (with plain Docker Compose, filling in the variables Coolify would). On a real Coolify v4 instance all six images built and the stack started; the problems met there are listed under [Troubleshooting](#troubleshooting). After those fixes a real installation on Coolify v4 ran end to end through sign-in, creating projects in the dashboard and connecting Claude over MCP (OAuth). Event collection on that installation was not checked by us — please report what you find.

---

## Contents

1. [What gets deployed](#what-gets-deployed)
2. [Before you start](#before-you-start)
3. [Step 1 — Create the resource](#step-1--create-the-resource)
4. [Step 2 — Environment variables](#step-2--environment-variables)
5. [Step 3 — Domains](#step-3--domains)
6. [Step 4 — First deploy](#step-4--first-deploy)
7. [Step 5 — Sign in and check](#step-5--sign-in-and-check)
8. [Step 6 — Send your first events](#step-6--send-your-first-events)
9. [Connecting Claude or ChatGPT](#connecting-claude-or-chatgpt)
10. [Storage: server disk or S3](#storage-server-disk-or-s3)
11. [Backups](#backups)
12. [Updating](#updating)
13. [Troubleshooting](#troubleshooting)
14. [Reference: every variable](#reference-every-variable)

---

## What gets deployed

| Service | What it does | Public domain | Internal port |
|---|---|---|---|
| `control` | Dashboard (web UI), admin API, MCP endpoint for AI assistants | **yes** — e.g. `analytics.example.com` | 8090 |
| `ingest` | Receives events from your websites and apps (the SDK "host") | **yes** — e.g. `in.analytics.example.com` | 8080 |
| `query` | Answers analytics queries for the dashboard | no | — |
| `shipper` | Turns received events into columnar files | no | — |
| `compactor` | Merges files, applies retention and deletion requests | no | — |
| `init` | Prepares the data folder, then exits | **no — never give it a domain** | — |
| `bootstrap` | Creates the first account on the first deploy, then exits | **no** | — |

`init` and `bootstrap` are one-off steps. Coolify shows them as *exited* after a deploy; that is expected.

Only `control` and `ingest` get a domain. All data lives in one Docker volume (`data`) unless you switch to S3.

---

## Before you start

- **Coolify v4** with a server that has the Coolify proxy (Traefik or Caddy) running.
- **A domain** whose DNS you control. You need two hostnames pointing at the server, for example:

  | Record | Type | Value |
  |---|---|---|
  | `analytics.example.com` | `A` | your server's IP |
  | `in.analytics.example.com` | `A` | your server's IP |

  A wildcard record (`*.example.com`) covers both.
- **Enough resources for the first build.** The query and storage services are written in Rust and compiled on your server the first time:

  | | First deploy | Later deploys (no change in `engine/`) |
  |---|---|---|
  | Build time | about **30 minutes** on 4 vCPU | a few minutes (Docker layer cache) |
  | Build memory | several GB; lower with `CARGO_BUILD_JOBS` | — |
  | Disk | about 10 GB free for the build cache | — |

  If the build is killed for lack of memory (`signal: 9`, `SIGKILL` or *"cannot allocate memory"* in the deploy log), set `CARGO_BUILD_JOBS=1` and deploy again. It is slower but needs much less memory.

- **Running** needs little: a few hundred MB idle. The query service may use up to `LA_QUERY_MEMORY_BYTES` (1 GiB by default) per running query.

---

## Step 1 — Create the resource

1. Open your project in Coolify → **+ New** → **Public Repository** (or your GitHub App / private key if you forked the repository).
2. Repository URL: `https://github.com/mucan54/izlek` (or your fork). Branch: `main`.
3. **Build Pack:** `Docker Compose`.
4. **Base Directory:** `/`
5. **Docker Compose Location:** `/docker-compose.coolify.yml`
6. Continue. Coolify reads the file and lists its services and variables. **Do not deploy yet.**

---

## Step 2 — Environment variables

Open the resource → **Environment Variables**. Coolify has already created the variables it found in the file.

### Required

| Variable | Value | Notes |
|---|---|---|
| `ADMIN_EMAIL` | your e-mail address | The first (owner) account. **Must have "Runtime" enabled.** Without it the stack builds but the dashboard does not start. |
| `LA_PUBLIC_URL` | `https://analytics.example.com` | The dashboard's public address, exactly as in the browser, no trailing slash. Used for sign-in links, SDK snippets and the OAuth sign-in of AI assistants. |
| `LA_PUBLIC_INGEST_URL` | `https://in.analytics.example.com` | Where SDKs send events. Shown in the SDK snippets. |

`LA_PUBLIC_URL` and `LA_PUBLIC_INGEST_URL` default to the domains Coolify generated. Set them explicitly once you use your own domains (Step 3); otherwise snippets and assistant connections may point at the generated addresses.

### Generated by Coolify — do not set

| Variable | What it is |
|---|---|
| `SERVICE_PASSWORD_ADMIN` | The first account's password. Read it here after the first deploy to sign in. |
| `SERVICE_PASSWORD_64_QUERYSECRET` | Shared secret between the dashboard and the query service. |
| `SERVICE_URL_CONTROL_8090`, `SERVICE_URL_INGEST_8080` | The generated addresses of the two public services. |

Coolify may show `SERVICE_PASSWORD_*` variables as build arguments in the deploy log. That is Coolify's own behaviour and harmless; the values are not stored in the images.

### Optional

| Variable | Default | When to change |
|---|---|---|
| `ORG_NAME`, `PROJECT_NAME` | `My organization`, `My project` | Names of the first organization and project (renameable later in the UI). |
| `LA_TRUSTED_PROXIES` | `1` | Set `2` if Cloudflare (proxied, orange cloud) or another CDN sits in front of Coolify. Wrong values make every visitor look like the same IP, which breaks per-IP rate limits and country detection. |
| `CARGO_BUILD_JOBS` | all cores | `1` or `2` on small servers (see above). |
| `LA_QUERY_MEMORY_BYTES` | `1073741824` (1 GiB) | Memory budget of one query. Lower it on a small server. |
| `LA_QUERY_CACHE_BYTES` | `2147483648` (2 GiB) | Local disk cache of data files for the query service. |
| `LA_STORE_URL`, `LA_S3_*` | server disk | See [Storage](#storage-server-disk-or-s3). |

Every variable the services read is set **Runtime**. Only `CARGO_BUILD_JOBS` is used at build time.

---

## Step 3 — Domains

Open the resource → **Domains** (or the per-service settings, depending on your Coolify version).

Coolify generates an address for `control` and `ingest`. Replace them with your own:

| Service | Domain | Internal port |
|---|---|---|
| `control` | `https://analytics.example.com` | **8090** |
| `ingest` | `https://in.analytics.example.com` | **8080** |
| `init`, `bootstrap`, `query`, `shipper`, `compactor` | *(none)* | — |

How to enter them:

- Some Coolify versions take the port as a suffix in the domain field (`https://analytics.example.com:8090`), others have a separate *Internal port* column. Use whichever your version shows. The suffix only tells the proxy which container port to use; visitors still open plain `https://analytics.example.com`.
- **Do not attach a domain to `init` or `bootstrap`.** They exit right after starting, so the proxy has nothing to route to (Coolify shows a warning icon in the *Internal port* column).
- **One address for the dashboard.** If you also want `www.analytics.example.com`, add it to `control` with **Domain redirect** to the main address, or leave it out. Sign-in cookies and the AI-assistant connection are tied to a single address (`LA_PUBLIC_URL`).
- Leave **HTTP → HTTPS** on for both. Coolify obtains the certificates.

After changing domains, update `LA_PUBLIC_URL` and `LA_PUBLIC_INGEST_URL` to match (Step 2).

---

## Step 4 — First deploy

Press **Deploy**. What you should see in the deploy log:

1. Six images are built: `control`, `bootstrap`, `ingest` (Go, a minute or two) and `shipper`, `compactor`, `query` (Rust, about 30 minutes the first time; `Compiling datafusion …` lines are normal).
2. `init` starts and exits.
3. `bootstrap` starts and exits **with code 0**.
4. `control`, `ingest`, `query`, `shipper`, `compactor` start and become **healthy** within about a minute.

If the deploy stops, see [Troubleshooting](#troubleshooting).

On the first deploy, `bootstrap` creates the owner account, an organization, a project, a write token and a secret key, and prints the token and the key **once** in its log. Later deploys find everything in place and change nothing.

---

## Step 5 — Sign in and check

1. Open `https://analytics.example.com`.
2. Sign in with `ADMIN_EMAIL` and the value of `SERVICE_PASSWORD_ADMIN` from **Environment Variables**.
3. Go to **Account** and change the password. Optionally enable two-step verification there. The language (English / Turkish) can be switched on the sign-in screen and under **Account**.
4. Check that event collection is reachable: `https://in.analytics.example.com/healthz` should answer `ok`.
5. Housekeeping: the secret key printed by `bootstrap` appeared in a deployment log. Go to **Project settings → Secret keys**, create the keys you actually need and delete the first one.

---

## Step 6 — Send your first events

**Project settings → SDK setup** shows ready-to-paste snippets with your write token and the ingestion address. For a website:

```html
<script src="izlek.min.js"></script>
<script>
  izlek.init({ token: 'pk_…', host: 'https://in.analytics.example.com' })
</script>
```

The SDK files are not published to npm yet: build them from `sdk-web/` (`npm ci && npm run build`) and serve `dist/izlek.min.js` from your site.

Or use an existing PostHog client unchanged — point it at the ingestion address and use your write token as the API key:

```js
posthog.init('pk_…', { api_host: 'https://in.analytics.example.com' })
```

Or test from a terminal:

```sh
curl -H 'Content-Type: application/json' https://in.analytics.example.com/batch/ \
  -d '{"api_key":"pk_…","batch":[{"event":"hello","distinct_id":"test-user"}]}'
```

Events appear under **Event stream** after 15–60 seconds.

If your website is on another domain, add it under **Project settings → Write tokens → allowed origins** (or leave the list empty to accept any origin).

---

## Connecting Claude or ChatGPT

The dashboard includes an MCP server, so an AI assistant can create projects and tokens, hand you SDK snippets, check whether events arrive, manage feature flags and answer analytics questions.

Requirements on Coolify:

- `LA_PUBLIC_URL` set to the dashboard's **https** address (Step 2). With a plain `http://` value the dashboard still starts, but assistant sign-in (OAuth) is switched off and only secret keys work.
- The dashboard reachable from the internet (Anthropic's and OpenAI's servers connect to it).

The endpoint is `https://analytics.example.com/mcp`. **Project settings → AI assistants** shows it together with step-by-step instructions for Claude Code, claude.ai / Claude Desktop and ChatGPT. Approved connections are listed (and can be revoked) under **Account → Connected apps**. Details: [`docs/mcp.md`](../../docs/mcp.md).

---

## Storage: server disk or S3

**Default — server disk.** All data lives in the `data` volume: events, the ingestion write-ahead log, the query cache and the dashboard's SQLite database. Nothing else to configure.

**S3-compatible storage.** Keeps events in a bucket instead (cheaper to grow, easier to back up). Set:

```
LA_STORE_URL=s3://your-bucket/izlek
LA_S3_ENDPOINT=https://s3.example.com      # leave empty for AWS
LA_S3_REGION=us-east-1
LA_S3_ACCESS_KEY=…
LA_S3_SECRET_KEY=…
```

- **Check your provider first.** izlek relies on conditional writes (`PUT` with `If-None-Match: *`). If a provider silently ignores that header, two services can overwrite each other's bookkeeping. MinIO is tested in CI; for anything else run the contract tests from the [main README](../../README.md#several-machines) once.
- The `data` volume must stay: it still holds the write-ahead log (accepted events not uploaded yet) and the dashboard database.
- **Decide before you collect real data.** Switching later does not move existing events from the disk to the bucket.

---

## Backups

Coolify's persistent storage is **not** a backup.

- **Server disk mode:** back up the `data` volume (Coolify → **Storages**, or your server's volume backups).
- **S3 mode:** enable versioning or replication on the bucket, and back up the dashboard database, which still lives in the volume at `/data/control/control.db`. A consistent copy can be made while running: `docker exec <control container> /app backup /data/control/backup.db`.

If the dashboard database is lost, event collection and stored data keep working, but users, tokens, flags and saved insights must be recreated.

---

## Updating

Press **Redeploy** (or enable automatic deploys on push).

- Only images whose sources changed are rebuilt. Changes under `engine/` mean a long Rust build again.
- During the switch, ingestion seals and uploads what it has buffered (`stop_grace_period: 90s`). If the proxy cuts it short nothing is lost: buffered events stay in the volume and are uploaded when the new container starts.
- With a single ingestion container, SDKs see a short interruption and retry by themselves.
- `bootstrap` runs again and changes nothing.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Deploy fails immediately: `required variable ADMIN_EMAIL is missing a value` | An old version of the compose file | Redeploy from the latest `main`. |
| Deploy log: `service "bootstrap" didn't complete successfully: exit 2` | `ADMIN_EMAIL` is empty **at runtime** | Set `ADMIN_EMAIL` with **Runtime** enabled, then redeploy. The `bootstrap` container's log says "the owner's e-mail is empty". |
| `bootstrap` exits with code 1 | Usually storage problems (S3 credentials, bucket missing) or an unwritable volume | Read the `bootstrap` container's log. |
| Build killed, `signal: 9` / out of memory | Rust build needs more memory than the server has | `CARGO_BUILD_JOBS=1`, redeploy. Adding swap also helps. |
| Warning icon in *Internal port* next to a domain | Domain attached to `init` (or another service without a port) | Remove it; give domains only to `control` (8090) and `ingest` (8080). |
| `404 page not found` / `no available server` on a domain | Wrong port or the service is not healthy yet | Check the port mapping (Step 3) and the service's status and log. |
| Dashboard opens, but SDK snippets show a `…invalid` or generated host | `LA_PUBLIC_INGEST_URL` not set | Set it to your ingestion domain, redeploy. |
| Claude / ChatGPT cannot connect, or the dashboard log says `LA_PUBLIC_URL ignored` | `LA_PUBLIC_URL` missing, wrong, or `http://` | Set it to the exact `https://` dashboard address, redeploy. |
| Sign-in fails right after deploy | Using the wrong password | The password is `SERVICE_PASSWORD_ADMIN` from **Environment Variables**, unless you changed it under Account. |
| All visitors show the same country, or rate limits hit everyone | `LA_TRUSTED_PROXIES` does not match your proxy chain | `1` behind Coolify only, `2` behind Cloudflare (proxied) + Coolify. |
| Events accepted (`200`) but nothing in **Event stream** | Normal delay is 15–60 s; otherwise `shipper` or `query` is not running | Check that `shipper` and `query` are healthy and read their logs. |

The containers have no shell. To look inside, use their logs; health is checked by a small built-in probe (`/la-healthcheck`).

---

## Reference: every variable

| Variable | Service(s) | Default |
|---|---|---|
| `ADMIN_EMAIL` | bootstrap | — (required) |
| `ORG_NAME`, `PROJECT_NAME` | bootstrap | `My organization`, `My project` |
| `LA_PUBLIC_URL` | control | the generated control address |
| `LA_PUBLIC_INGEST_URL` | control | the generated ingest address |
| `LA_TRUSTED_PROXIES` | control, ingest | `1` |
| `LA_COOKIE_SECURE` | control | `true` (set `false` only for a plain-HTTP test) |
| `LA_STORE_URL` | all | `file:///data/store` |
| `LA_S3_ENDPOINT`, `LA_S3_REGION`, `LA_S3_ACCESS_KEY`, `LA_S3_SECRET_KEY` | all | empty |
| `LA_SHIPPER_BATCH_MB` | shipper | `64` |
| `LA_QUERY_MEMORY_BYTES` | query | `1073741824` |
| `LA_QUERY_CACHE_BYTES` | query | `2147483648` |
| `CARGO_BUILD_JOBS` | build only | all cores |
| `SERVICE_PASSWORD_ADMIN`, `SERVICE_PASSWORD_64_QUERYSECRET`, `SERVICE_URL_CONTROL_8090`, `SERVICE_URL_INGEST_8080` | generated by Coolify | — |

Advanced settings for each component are listed in the component READMEs ([ingest](../../ingest/README.md), [shipper](../../engine/shipper/README.md), [query](../../engine/query/README.md), [control](../../control/README.md)); add them to the `environment` of the relevant service in your fork of the compose file.

## What this setup does not give you

- **More than one server.** For several ingestion or query nodes, run the images yourself with a shared S3 bucket (see the main README).
- **Metrics scraping.** The services expose Prometheus metrics on internal ports (`ingest:9100`, `control:9101`, `shipper:9102`, `compactor:9103`, `query:8081`). Add a Prometheus container to the stack to use the alert rules in `deploy/prometheus/alerts.yml`.
- **Country detection.** Without a GeoIP database every event's country is unknown. Mount a country MMDB file into `ingest` and set `LA_GEO_DB` to its path.
