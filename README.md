# chino-api

Backend BFF for the **chino** product of the zaentrum platform. A read-only
Go API that fronts the catalog and streaming services for the chino web, mobile,
and TV clients: browse, search, continue-watching, watchlists, playback
progress, telemetry, and a bug-report pipeline into OpenProject.

## Stack

- Go + chi router + `slog`
- OIDC JWT validation via `go-oidc`
- Postgres (pgx) for user-state (playback progress, watch history, watchlists)
- Prometheus metrics
- Distroless container image

chino-api is a pure read consumer: it talks to the catalog service for metadata
and `chino-stream` for playback bytes. Writes never originate here.

## Endpoints

The full surface is the embedded OpenAPI spec, served at `GET /api/openapi.yaml`
and rendered from `internal/http/openapi.yaml`. Highlights:

| Path | Auth | Notes |
|---|---|---|
| `GET /api/healthz` | none | liveness / readiness |
| `GET /api/openapi.yaml` | none | OpenAPI 3 spec |
| `GET /api/config` | none | client app config |
| `GET /api/v1/me` | bearer JWT | echoes the caller's `sub` |
| `GET /api/v1/items` | bearer JWT | catalog browse |
| `GET /api/v1/items/{id}` | bearer JWT | item detail, cast and crew by role |
| `GET /api/v1/people` | bearer JWT | people search by name |
| `GET /api/v1/people/{id}` | bearer JWT | a person, their details and filmography |
| `GET /api/v1/people/{id}/profile` | bearer JWT or stream token | a person's portrait (`profile_url`) |
| `GET /api/v1/me/continue-watching` | bearer JWT | resume list |
| `GET /api/v1/me/watchlists` | bearer JWT | named watchlists |
| `GET/POST /api/v1/items/{id}/progress` | bearer JWT | playback progress |
| `POST /api/v1/play/events` | bearer JWT or stream token | playback telemetry (the stream token for `sendBeacon`) |
| `GET /api/v1/events` | bearer JWT or stream token | live catalog notifications (SSE) |
| `POST /api/v1/feedback` | bearer JWT | bug report → OpenProject (503 when unconfigured) |
| `GET /api/v1/items/{id}/play/info` | bearer JWT or stream token | how the item plays for the client's `caps`; a packaged title's `qualities` |
| `GET /api/v1/items/{id}/play/master.m3u8` | bearer JWT or stream token | the HLS master for the client's `caps` and `q` |
| `POST /api/v1/items/{id}/play/prewarm` | bearer JWT or stream token | warm the variant the client starts on |
| `GET /api/v1/items/{id}/play/{vN\|aN\|sN}/playlist.m3u8` | bearer JWT or stream token | packaged video / audio / WebVTT rendition, its segments next to it |

### Playback

The play routes proxy chino-stream, query and all: the stream token, `caps`
(what the client decodes, e.g. `avc:2160,hvc:2160,aac,eac3`) and `q` ride on
to every playlist and segment. For a packaged title chino-stream serves each
client one codec family (HEVC when it decodes it, else H.264) at the heights
its decoder takes, the audio groups it decodes and the subtitle group the
package has; `q=<name>` from `/play/info`'s `qualities` serves one rung, `auto`
(the default) the ladder. The rules and the `/play/info` shape are in
chino-stream's README and in the spec (`PlayInfo`, `PlayQuality`). A quality
menu shows `qualities` when it has two or more entries, by `label`, and
reloads the master with `q=<name>`.

## Local development

```bash
go run ./cmd/server
# in another shell
curl -sS http://localhost:8080/api/healthz
```

Disable OIDC for local poking:

```bash
OIDC_ENABLED=false go run ./cmd/server
curl -sS http://localhost:8080/api/v1/items
```

Where a client cannot set an `Authorization` header (an `<img>` or `<video>`
URL, `sendBeacon`, an `EventSource`), it authenticates with `?stream=<token>`,
a stream token from `POST /api/v1/me/stream-token`. The bearer in the URL
(`?token=`) is deprecated: it is still accepted in this release and goes in
the next. The request log shows credential values as `REDACTED`.

When `PG_URL` is empty, progress and telemetry endpoints answer gracefully but
do not persist — keeps local dev simple. When `OPENPROJECT_TOKEN` is empty the
feedback endpoint answers 503 and clients keep the feature off.

## Configuration

Configured entirely through environment variables (see `internal/config`):

| Var | Purpose |
|---|---|
| `ADDR` | listen address (default `:8080`) |
| `OIDC_ISSUER` / `OIDC_AUDIENCE` / `OIDC_ENABLED` | OIDC JWT validation |
| `KATALOG_BASE_URL` | catalog metadata service |
| `STREAM_BASE_URL` | chino-stream (HLS / trickplay / play info) |
| `ARTWORK_BASE_URL` / `ANALYZER_BASE_URL` | artwork + packaging admin surface |
| `PG_URL` | Postgres URL for user-state (optional) |
| `ADMIN_SUBJECTS` | comma-separated OIDC `sub` claims allowed on `/api/v1/admin/*` |
| `OPENPROJECT_URL` / `OPENPROJECT_TOKEN` / `OPENPROJECT_PROJECT_ID` / `OPENPROJECT_BUG_TYPE_ID` | feedback pipeline (optional) |
| `STREAM_SIGNING_KEY` | shared HMAC secret for signed `?stream=` URLs (optional) |

## Layout

```
cmd/server/main.go                  process entry
internal/config/                    env wiring
internal/http/router.go             chi router + middleware
internal/http/openapi.{go,yaml}     embedded OpenAPI spec
internal/http/                      browse / watchlists / progress / feedback / ... handlers
internal/auth/                      Bearer JWT verifier + stream-token middleware
internal/katalog/                   catalog + people read clients
internal/store/                     Postgres store + SQL migrations
internal/openproject/               minimal OpenProject client (feedback)
internal/metrics/                   Prometheus surface
k8s/                                sample Deployment / Service / Route / ServiceMonitor / Dashboard
Dockerfile                          distroless multi-stage build
```

## Build the container

```bash
docker build -t zaentrum/chino-api .
```

The `k8s/` manifests are samples. Build and push the image to your own registry,
adjust the env values and hostnames for your environment, and deploy.

## License

[MPL-2.0](LICENSE).
