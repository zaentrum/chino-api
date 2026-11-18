# chino-api

chino-api is the backend of the chino apps: the web, mobile and TV clients
call it, at `/api` of the platform's address, for browsing, search, a title's
detail and playback, Continue Watching, watchlists, progress, notices and bug
reports. It reads the catalog from katalog-api, proxies playback, subtitles
and fonts to chino-stream, and asks katalog-manager for artwork, the subtitle
languages and an admin's packaging, and portal-api for notices and account
deletion. It keeps what is the viewer's own (progress, watch history,
watchlists, likes) in PostgreSQL, mints the stream tokens chino-stream and
katalog-manager check, and counts the players' telemetry for Prometheus.
How a change is proposed and what a pull request has to pass is in the
[guide to contributing](https://github.com/zaentrum/.github/blob/main/CONTRIBUTING.md);
what may be built is in [building from the specification](https://github.com/zaentrum/zaentrum/blob/main/AGENTS.md).

## Build

```sh
go build ./...
docker build -t chino-api .
```

It needs Go 1.24 (`go.mod`). The image builds a static Linux amd64 binary in
`golang:1.24-alpine` and runs it on `gcr.io/distroless/static-debian12:nonroot`
as a non-root user, on port 8080. `--build-arg BASE=<registry>/library/` pulls
the build image from a mirror; the runtime image is the same whatever `BASE`
says.

## Test

```sh
go vet ./...
CHINO_API_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

The workflow `.github/workflows/build.yml` runs `go build ./...`,
`go vet ./...` and `go test -race ./...` on every pull request and on main, with a
throwaway PostgreSQL 16 for the tests that need one. The tests
of what chino-api keeps (progress, watched, finished titles, Continue
Watching, account deletion and the writes refused after it, a capped viewer's
lists, the account's settings, a database error's answer, lists by kind: 35
of the 186) need a
PostgreSQL in which they may create and drop schemas, named by
`CHINO_API_TEST_DATABASE_URL`; each works in a schema of its own and drops it.
Without it they are skipped, and where `CI` is set (GitHub's runners set it)
they fail, so that the workflow cannot pass without them. Every other
service is a fake the tests start with `httptest`: katalog-api,
chino-stream, katalog-manager, portal-api and the OIDC issuer.

## Run it

```sh
OIDC_ENABLED=false KATALOG_BASE_URL=http://localhost:8081 \
  STREAM_BASE_URL=http://localhost:8082 ARTWORK_BASE_URL=http://localhost:8083 \
  KATALOG_MANAGER_URL=http://localhost:8083 PORTAL_BASE_URL=http://localhost:8084 \
  go run ./cmd/server
curl -fsS http://localhost:8080/api/config
```

It listens on `ADDR` (`:8080`). The upstreams' default URLs name one
installation's services, so a local run sets each; here katalog-api,
chino-stream and katalog-manager run on this machine (their `AGENTS.md` say
how) and portal-api does not. `OIDC_ENABLED=false` lets a request through
without a bearer: `/api/config`, the play routes and the subtitle languages
(from a katalog-manager with `AUTH_DISABLED=true`) then answer, the catalog
routes answer 502 (katalog-api asks for a bearer of its issuer on its catalog
routes and on `/api/v1/holdings`), and `/api/v1/me` 401. With it on, a bearer of `OIDC_ISSUER` for
`OIDC_AUDIENCE` (`chino-web`) is checked and passed on upstream. `PG_URL` names the database for what chino-api keeps (it applies its
migrations at start); empty, progress and telemetry answer and keep nothing.
`STREAM_SIGNING_KEY` (base64, at least 16 bytes) is the key chino-stream and
katalog-manager check its stream tokens with. `KAFKA_BROKERS` feeds
`/api/v1/events`, which sends heartbeats only without it; `OPENPROJECT_TOKEN`
turns bug reports on. There is no mock server and no recorded answers. The
reference demo runs chino-api at `/api` of its address with its upstreams
inside its cluster ([how it is deployed](https://github.com/zaentrum/zaentrum/blob/main/docs/reference-demo.md)).

## What the tests leave unchecked

- The Kafka tail behind `/api/v1/events`: no test publishes through the
  broker or reads a topic. Tested are the topics' names, the wait for a topic,
  a message made into a note, and that the handler opens with `: connected`.
- Bug reports: four tests post one to `POST /api/v1/feedback`, against a
  fake OpenProject that fails it, one that redirects it, and two that file
  it and fail its screenshot; the OpenProject client (`internal/openproject`)
  has no test of its own. No test fails the database where a report's
  fingerprint is kept, or where an account's data is deleted after
  portal-api deleted the account: those two lines' levels (`WARN`, `ERROR`)
  are unchecked.
- Named watchlists: creating one runs in one test, through the store, as a
  fixture; renaming and deleting one, and the routes that create, rename and
  delete (`POST /api/v1/me/watchlists`, `PATCH` and `DELETE`
  `/api/v1/me/watchlists/{listId}`), have no test.
- A real identity provider: the tests' issuer is a fake.
- `internal/http/openapi.yaml`, served at `/api/openapi.yaml`: no test
  compares it with the routes; one checks that it lists the settings
  store's codes and the lists' `kind` and `include`, and one that its
  `AppConfig` has the fields `/api/config` answers, `features` among them,
  and no other.
- The process's start (`cmd/server`) has no test.
