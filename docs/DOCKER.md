# Running MMXC in a container

MMXC ships as a static Go binary in a small Alpine image with Poppler's
`pdftotext` for PDF statement extraction. It uses a pure-Go SQLite driver, so
there is no C toolchain and the same Dockerfile builds for `amd64` and `arm64`.
These instructions work with Docker or Podman (`podman` and `podman compose` are
drop-in replacements for the commands below).

There is no published image yet; you build it from this repository.

## What you provide at runtime

- **`mmxc.yaml`** — your config. Copy `mmxc.example.yaml` and set at least
  `db_path`. Mount it **read-write**: the Settings page saves changes back to this
  file (and tightens it to mode `0600`, which only the file's owner may do — see
  [File permissions](#file-permissions)). Mount it read-only (`:ro`) only if you
  want to manage the config by hand; saving on the Settings page then fails.
- **`data/`** — a folder holding your MMEX `.mmb` database plus the backups,
  audit log and statement archive MMXC creates. `db_path`, `backup_dir`,
  `audit_dir` and `statement_dir` in the config must resolve **inside** this
  folder, e.g. `db_path: data/family.mmb` (the container's working directory is
  `/app`, so `data/…` is `/app/data/…`).

> The `.mmb` is your real money data. MMXC refuses to write while another program
> (such as Money Manager Ex) holds the file locked, but don't run both at once.
> Whether MMXC may edit the database is not a setting: it follows the file's own
> write bits. Start with `chmod a-w data/family.mmb` to run read-only until you've
> confirmed everything reads correctly. Keep your own backups too.

## Quick start

```sh
cp mmxc.example.yaml mmxc.yaml          # edit: set db_path (e.g. data/family.mmb)
mkdir -p data && cp /path/to/your.mmb data/
docker compose up -d --build
```

Then open <http://localhost:8080>. With no database in `data/`, MMXC shows a
first-run page that creates a new one.

`docker-compose.yml` in the repository root builds the image, maps port 8080, and
mounts `./mmxc.yaml` and `./data`. Set `TZ` in it to your timezone so dates
render in local time.

Without compose:

```sh
docker build -t mmxc:latest .
docker run -d --name mmxc --restart unless-stopped -p 8080:8080 \
  -e TZ=Europe/Berlin \
  -v "$PWD/mmxc.yaml:/app/mmxc.yaml" \
  -v "$PWD/data:/app/data" \
  mmxc:latest
```

To build for another architecture (needs buildx):

```sh
docker buildx build --platform linux/arm64 -t mmxc:latest --load .
```

## File permissions

The container runs as UID **1000**. The bind-mounted `data/` folder and
`mmxc.yaml` must be writable by that UID (and `mmxc.yaml` owned by it), or
backups, the audit log, statement uploads and saving settings fail. Either:

- `chown -R 1000 data mmxc.yaml` on the host, **or**
- run the container as the user that owns the folder: set `user: "UID:GID"` in
  the compose file (or `--user UID:GID`). With rootless Podman, `--userns=keep-id`
  does the same.

## Reaching your LLM from the container

Inside the container, `localhost` is the container itself, not your machine. If
Ollama runs on the Docker host, set `llm_base_url` accordingly in `mmxc.yaml`:

- Docker Desktop (macOS/Windows): `http://host.docker.internal:11434/v1`
- Linux: add `extra_hosts: ["host.docker.internal:host-gateway"]` to the service
  (or `--add-host=host.docker.internal:host-gateway`) and use the same URL. Ollama
  must listen on an address the container can reach (`OLLAMA_HOST=0.0.0.0`), not
  just loopback.
- Or use the host/LAN name of the machine running the model.

Remember to start Ollama with a context window large enough for a statement
(`OLLAMA_CONTEXT_LENGTH=32768`).

## Environment variables

Boot-time settings can come from the environment instead of (or overriding) the
YAML file: `MMXC_CONFIG`, `MMXC_DB_PATH`, `MMXC_HTTP_ADDR`, `MMXC_DB_PASSWORD`,
`MMXC_DB_PASSWORD_FILE`, `MMXC_AUTH_ENABLED`, `MMXC_OIDC_*`, `MMXC_SESSION_*`.
See the table in the [README](../README.md#configuration) and the commented
examples in `docker-compose.yml`. Everything else is configured in `mmxc.yaml` or
on the Settings page.

### Encrypted databases

For an encrypted `.mmbx` database, prefer a secret file over the plain env var, so
the password stays out of `docker inspect` and off the disk it protects
(`/run/secrets` is tmpfs):

```yaml
services:
  mmxc:
    environment:
      MMXC_DB_PASSWORD_FILE: /run/secrets/mmxc-db-password
    secrets:
      - mmxc-db-password
secrets:
  mmxc-db-password:
    file: ./secrets/mmxc-db-password     # keep this outside ./data
```

Without a password configured, MMXC serves an unlock page at startup and asks for
it there; it keeps the password in memory only.

## HTTPS and sign-in

MMXC speaks plain HTTP. If it is reachable beyond your own machine, put a reverse
proxy (Caddy, nginx, Traefik, your NAS's built-in proxy, …) in front of it for TLS
and a hostname, and consider enabling the OIDC sign-in (see `mmxc.example.yaml`).
Behind a TLS-terminating proxy set, in `mmxc.yaml`:

```yaml
trust_forwarded_proto: true
```

so the session cookie's `Secure` flag is set from `X-Forwarded-Proto`. With OIDC
enabled, `oidc_redirect_url` must be the public HTTPS URL ending in
`/auth/callback`.

The image's `HEALTHCHECK` requests `/`. When auth is enabled `/` redirects to the
identity provider, so drop or relax the healthcheck (e.g. point it at
`/favicon.svg`).

### Trusting a private CA (OIDC over an internal HTTPS endpoint)

If your OIDC provider is served over HTTPS by your own certificate authority, the
container doesn't trust it out of the box (the image only carries the public CA
bundle), and startup fails with:

```
init auth: OIDC discovery from https://id.example.internal: ... x509: certificate signed by unknown authority
```

Give the container your CA's **root certificate** (PEM, plus any intermediate).
Go reads every PEM file in the directories named by `SSL_CERT_DIR` *in addition
to* the public bundle, so public HTTPS keeps working:

```yaml
    environment:
      SSL_CERT_DIR: /certs
    volumes:
      - ./certs:/certs:ro           # contains your-ca.crt (PEM)
```

Alternatively, bake it into the image (permanent, but needs a rebuild when the CA
rotates) by adding this to the runtime stage of the `Dockerfile`, before
`USER mmxc`:

```dockerfile
COPY certs/your-ca.crt /usr/local/share/ca-certificates/your-ca.crt
RUN update-ca-certificates
```

## Updating

```sh
git pull
docker compose up -d --build
```

Your data lives in `./data`, so recreating the container is safe. MMXC takes a
full backup of the database into `backup_dir` before every change it makes.
