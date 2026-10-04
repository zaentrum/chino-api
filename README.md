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
| `POST /api/v1/admin/items/{id}/package` | bearer JWT with the admin role | an item's packaging, forwarded to katalog-manager with the bearer |
| `GET /api/v1/admin/items/{id}/package` | bearer JWT with the admin role | the item's processing steps, from katalog-manager |
| `GET /api/v1/items/{id}/play/info` | bearer JWT or stream token | how the item plays for the client's `caps`; a packaged title's `qualities` |
| `GET /api/v1/items/{id}/play/master.m3u8` | bearer JWT or stream token | the HLS master for the client's `caps` and `q` |
| `POST /api/v1/items/{id}/play/prewarm` | bearer JWT or stream token | warm the variant the client starts on |
| `GET /api/v1/items/{id}/play/{vN\|aN\|sN}/playlist.m3u8` | bearer JWT or stream token | packaged video / audio / WebVTT rendition, its segments next to it |

### Parental controls

A viewer whose access token carries `max_rating`, a whole number of years, is
capped at that age (a kid's account); a token without the claim is not, and a
claim that is no whole number of years holds its viewer to the strictest cap,
0 (chino-api says so once in its log). chino-api passes the cap to katalog-api
as `max_rating` on every catalog request, so the lists, search, people,
continue watching and history are served what the cap allows. A stream token
minted for a capped viewer carries the cap in its user part
(`<subject>;max_rating=<age>`, signed with the rest; chino-stream and
katalog-manager verify it as before), so the media routes hold the viewer to
it too.

Every route of one title (detail, segments, more like this, subtitles, a
series' episodes and next episode, artwork, playback, trickplay, a sidecar
subtitle by the title it belongs to, and the viewer's own progress, watched,
watchlist and like of the title) answers a capped viewer 404 "not found" for a
title its cap does not allow, and for an id that names none, before anything
goes upstream: it asks katalog-api's `GET /api/v1/visible`, kept for 30
seconds. The lists chino-api keeps (watchlists and their counts, likes,
memberships) and chino-stream's Zap feed and packaged ids leave such titles
out. When katalog-api cannot say what a cap allows, a capped viewer is served
nothing (502). A viewer without a cap is served as before. Items carry
`min_age`, `certification` and `certification_country` as katalog-api sends
them, for a badge.

The tests of chino-api's own lists need a PostgreSQL in which they may create
and drop schemas, named by `CHINO_API_TEST_DATABASE_URL`, and are skipped
without it.

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
| `ARTWORK_BASE_URL` | katalog-manager, for artwork |
| `KATALOG_MANAGER_URL` | katalog-manager, where the admin packaging routes go with the admin's bearer (default `http://katalog-manager-api`; `ANALYZER_BASE_URL`, its former name, is read when it is unset) |
| `PG_URL` | Postgres URL for user-state (optional) |
| `ADMIN_ROLE` | the realm role (`realm_access.roles` of the access token) that opens `/api/v1/admin/*` (default `zaentrum-admin`, as katalog-manager and the portal) |
| `ADMIN_SUBJECTS` | deprecated: comma-separated OIDC `sub` values let through `/api/v1/admin/*` besides the role; empty by default, and it goes in a coming release |
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
