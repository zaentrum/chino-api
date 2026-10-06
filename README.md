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
| `DELETE /api/v1/me` | bearer JWT (in the header) | deletes the signed-in person's data and their account ([below](#deleting-an-account)) |
| `GET /api/v1/items` | bearer JWT | catalog browse |
| `GET /api/v1/items/{id}` | bearer JWT | item detail, cast and crew by role, its trailers and extras |
| `GET /api/v1/people` | bearer JWT | people search by name |
| `GET /api/v1/people/{id}` | bearer JWT | a person, their details and filmography |
| `GET /api/v1/people/{id}/profile` | bearer JWT or stream token | a person's portrait (`profile_url`) |
| `GET /api/v1/me/continue-watching` | bearer JWT | resume list |
| `GET /api/v1/me/watchlists` | bearer JWT | named watchlists |
| `GET/POST /api/v1/items/{id}/progress` | bearer JWT | playback progress |
| `POST /api/v1/play/events` | bearer JWT or stream token | playback telemetry (the stream token for `sendBeacon`) |
| `GET /api/v1/events` | bearer JWT or stream token | live catalog notifications (SSE) |
| `GET /api/v1/notices` | bearer JWT | what addons told the signed-in person, from portal-api — best effort ([below](#notices)) |
| `POST /api/v1/notices/{noticeId}/read` · `POST /api/v1/notices/read-all` · `DELETE /api/v1/notices/{noticeId}` | bearer JWT | one notice read, all read, one deleted — forwarded to portal-api |
| `POST /api/v1/feedback` | bearer JWT | bug report → OpenProject (503 when unconfigured) |
| `POST /api/v1/admin/items/{id}/package` | bearer JWT with the admin role | an item's packaging, forwarded to katalog-manager with the bearer |
| `GET /api/v1/admin/items/{id}/package` | bearer JWT with the admin role | the item's processing steps, from katalog-manager |
| `DELETE /api/v1/admin/accounts/{sub}/data` | bearer JWT with the admin role and the account deletion token | what chino keeps of an account an admin deletes on the portal's People page, for portal-api |
| `GET /api/v1/items/{id}/play/info` | bearer JWT or stream token | how the item plays for the client's `caps`; a packaged title's `qualities` |
| `GET /api/v1/items/{id}/play/master.m3u8` | bearer JWT or stream token | the HLS master for the client's `caps` and `q` |
| `POST /api/v1/items/{id}/play/prewarm` | bearer JWT or stream token | warm the variant the client starts on |
| `GET /api/v1/items/{id}/play/{vN\|aN\|sN}/playlist.m3u8` | bearer JWT or stream token | packaged video / audio / WebVTT rendition, its segments next to it |
| `GET /api/v1/items/{id}/extras/{extraId}/play/master.m3u8` | bearer JWT or stream token | an extra's HLS master, its `play_path` ([below](#extras)) |
| `GET /api/v1/items/{id}/extras/{extraId}/play/{vN\|aN\|sN}/playlist.m3u8` | bearer JWT or stream token | an extra's renditions, as a packaged title's |

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

### Deleting an account

A person deletes their own account from the app — the app stores ask every
app that makes accounts to offer it — with `DELETE /api/v1/me` and their
bearer, in the `Authorization` header (a `?token=` bearer is refused: a link
someone could be sent). chino-api deletes what it keeps of them — playback
progress, watch history, named watchlists and their items, likes, the legacy
watchlist rows — and asks portal-api, with the person's bearer and the
account deletion token (`ACCOUNT_DELETION_TOKEN`, sent as
`X-Account-Deletion-Token`), to delete their account in the platform's realm
(`DELETE /api/portal/me`). The rows are deleted in a transaction committed
only once portal-api has deleted the account, or found it gone already:

| Answer | When | Deleted |
|---|---|---|
| `200 {"account":"deleted","deleted":{…}}` | portal-api deleted the account | the data and the account; `deleted` counts the rows |
| `200 {"account":"gone",…}` | the account was gone already | the data |
| `409 {"error":"refused","message":…}` | portal-api refuses: the last admin, an administrator of Keycloak itself | nothing; `message` says why, for the person |
| `502 {"error":"account_not_deleted",…}` | portal-api did not answer, or refused the token | nothing: asking again starts over |
| `501 {"error":"account_deletion_unavailable",…}` | no token: accounts here live in an identity provider the platform does not manage | nothing |

The other direction: an admin deleting someone on the portal's People page has
portal-api ask for that person's data first, with the admin's bearer and the
same token: `DELETE /api/v1/admin/accounts/{sub}/data`. Bug reports already
sent to OpenProject are not deleted with an account, and the request log keeps
no bearer (it shows `REDACTED`).

A client offers it as **Delete Account**, asks first and says what goes
(their history, lists and progress, and their account), sends the request with
its bearer, and signs out on `200`; on `409` it shows `message`, on `501` that
accounts are deleted by whoever runs the server.

### Notices

An addon can tell one person something — "your title is ready". portal-api
keeps the notices and shows each person their own; the apps read and change
them here, and chino-api forwards the viewer's bearer to portal-api's
`/api/portal/me/notices` and keeps nothing of them. It is best effort, as the
slots are: `GET /api/v1/notices` always answers `200` —
`{"notices": [...], "unread": n, "available": true}`, or with no portal-api, or
one that does not answer, an empty list and `"available": false` — so a home
screen that shows notices never fails for them.

| Route | Answers |
|---|---|
| `GET /api/v1/notices` | the person's notices, newest first (they keep their newest 100), `unread`, `available` |
| `POST /api/v1/notices/{noticeId}/read` | `200 {"unread": n}`; reading it again keeps when it was first read |
| `POST /api/v1/notices/read-all` | `200 {"read": n, "unread": 0}` |
| `DELETE /api/v1/notices/{noticeId}` | `204` |

A notice the person does not have is `404 {"error":"not_found"}` — someone
else's is as one there is not; a change portal-api did not make is `502` (it
did not answer) or `503` (none configured), `{"error":"notices_unavailable"}`,
and the notice stays as it was. A notice carries `id`, `addon`, `addonTitle`
and `addonIcon` (whom it is from), `title` (at most 80 characters), `body` (at
most 280, line breaks allowed), `link` and `itemId` (`""` for none),
`createdAt`, and `readAt` (`null` while unread); the spec has the schema.

A client shows the title, the body and whom it is from as plain text, never as
markup; follows `link` only to its own server — a path, or an http(s) URL on
the server's origin; opens `itemId` with the item routes, which hold a capped
viewer to their cap; and shows the unread count where the person looks first.

### Playback

The play routes proxy chino-stream, query and all: the stream token, `caps`
(what the client decodes, e.g. `avc:2160,hvc:2160,aac,eac3`) and `q` ride on
to every playlist and segment, and so does `v=<versionId>`, which chino-stream
writes onto a package's URIs to pin the session to that version. For a
packaged title chino-stream serves each client one codec family (HEVC when it
decodes it, else H.264) at the heights its decoder takes, the audio groups it
decodes and the subtitle group the package has; `q=<name>` from `/play/info`'s
`qualities` serves one rung, `auto` (the default) the ladder. The rules and the
`/play/info` shape are in chino-stream's README and in the spec (`PlayInfo`,
`PlayQuality`). A quality menu shows `qualities` when it has two or more
entries, by `label`, and reloads the master with `q=<name>`.

### Extras

A movie's or a series' extras - its trailers, teasers, featurettes and other
bonus material, each a file of its own that the catalog has packaged for
streaming - come with its detail, `GET /api/v1/items/{id}`, beside
`trailers`, in the order a viewer sees them:

```json
{
  "id": "9c4e7a12-3b5d-4f60-8a91-0e2d4c6b8f13", "type": "movie", "title": "A Film",
  "trailers": [{"site": "YouTube", "external_id": "x1", "url": "https://www.youtube.com/watch?v=x1", "title": "Official Trailer"}],
  "extras": [
    {"id": "1b5c2a8e-6f0d-4c3e-9a51-2d7f0c4b8e01", "kind": "trailer", "title": "Trailer", "language": "en",
     "duration_ms": 33000, "local": true,
     "play_path": "/api/v1/items/9c4e7a12-3b5d-4f60-8a91-0e2d4c6b8f13/extras/1b5c2a8e-6f0d-4c3e-9a51-2d7f0c4b8e01/play/master.m3u8"}
  ]
}
```

- `trailers` stays what it was: the title's links to online videos, `url` and
  all. Installed clients read every entry as such a link (mobile and TV need
  `url`, every client opens it outside the app), so a trailer this server
  plays is never in it: it is an extra.
- An extra is `id`, `kind` (trailer, teaser, featurette, behind-the-scenes,
  making-of, deleted-scene, interview, gag-reel, short, other; a client skips
  kinds it does not know), `title` and, when known, `language` and
  `duration_ms`; a series' extra of one season has `season_number` (0 the
  specials). `local` is always `true`. `extras` is omitted when none plays, and
  while the catalog has no extras yet.
- `play_path` is the extra's HLS master. A client asks for it as for a title's
  master, with `?stream=<token>&caps=<caps>` (`q=<rung id>` for one rung), and
  the master's URIs carry the query on. chino-stream serves it from the
  extra's package, HEVC only as a title's is, and only under its own title; a
  client that decodes none of its rungs gets the on-the-fly master, transcoded
  from the package, its rung (`high`, `medium` or `low`) and audio tracks
  under the extra's own routes. Its routes are in the spec.
- The routes of a title's extras hold a capped viewer to the title's rating:
  the 404 of the title there too.
- An extra has no progress, watched, segments, trickplay, `/play/info` or
  `/play/prewarm`: a player of extras sends none of them, and Continue
  Watching is not touched.
- When an extra is packaged, `GET /api/v1/events` sends its title a note,
  phase `extra.packaged`, and the detail lists it on the next fetch. The
  extras' topic (`<prefix>catalog.extra.packaged`) is tailed by a consumer
  group of its own once it exists on the cluster, asked every minute until
  then, so a cluster not provisioned for extras yet loses none of the other
  notes.

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
| `ACCOUNT_DELETION_TOKEN` | the token chino-api and portal-api delete an account with (Secret `zaentrum-people`, key `deletion-token`, on the platform); empty: `DELETE /api/v1/me` answers 501 |
| `PORTAL_BASE_URL` | portal-api, for addon slots, notices and account deletion (default `http://portal-api`) |

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
