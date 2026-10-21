# Local API, client and CLI conventions

[Spec overview](../spec.md) · [Operational contracts](operations.md)

Normative detail for [spec.md "Local API, gateway and browsers"](../spec.md#local-api-gateway-and-browsers).
The Go types live in `api/apiv0`; the server in `api`, the client in `client`, the
daemon assembly in `node/daemon`. The served OpenAPI document is `GET /api/v0/openapi.json`
and must match the route table in `api.Routes` (a test enforces it).

## `$NU_PATH` layout

`$NU_PATH` defaults to `~/.nu`; any command initializes it on first use (`nu init` is
optional). Files are 0600, directories 0700.

| Entry | Contents |
|---|---|
| `config.toml` | Configuration; dotted keys, edited by `nu config get\|set <key> [value]`. `set` parses the value as TOML (`30`, `true`, `["a"]`, `"quoted"`) and falls back to a string. A running daemon applies changes after restart. |
| `node.key` | libp2p identity key (reserved; written by the networking layer) |
| `nu.db`, `blobs/` | SQLite database and blob directory (reserved for the store) |
| `api`, `gateway` | URL of the running daemon's listeners, written at start and removed at clean shutdown; clients discover the daemon through `api` |
| `daemon.lock` | flock held by the running daemon, containing its PID; a second daemon fails with `conflict` |
| `version` | layout version (`1`) |

Default config: `api.listen = "127.0.0.1:5171"`, `gateway.listen = "127.0.0.1:8171"`,
`swarm.listen = ["/ip4/0.0.0.0/udp/4171/quic-v1", "/ip6/::/udp/4171/quic-v1"]`. The
daemon refuses non-loopback API/gateway listeners until authentication exists.

## Error envelope and exit categories

Every error response is `{"error": {"code", "message", "action"?, "details"?}}`. Codes
are stable; messages and actions are plain language and may change. The CLI prints
`nu: <message>` and `  → <action>` (or the envelope on stderr with `--json`) and exits
with the code's category. Flag and argument mistakes exit 2 like Go's `flag` package.

| Code | HTTP | Exit | Meaning |
|---|---|---|---|
| `failed` | 500 | 1 | unclassified failure, failed operation |
| `invalid_argument` | 400 | 2 | malformed target, flag, cursor or body |
| `not_found` | 404 | 3 | unknown target, operation, route or config key |
| `not_supported` | 422 | 4 | valid input this version cannot act on yet (names, datasets, `nu://`, `at://`, `rasl://`) |
| `conflict` | 409 | 5 | daemon already running, operation already finished |
| `timeout` | 408 | 6 | a wait expired; `details.operation` holds the current snapshot |
| `unavailable` | 503 | 7 | no daemon reachable, or shutting down |
| `forbidden` | 403 | 8 | loopback/origin protections or authentication |
| `cancelled` | 409 | 9 | operation cancelled, command interrupted |
| `not_implemented` | 501 | 10 | endpoint exists but is not in this build |

## Paging

List endpoints take `?cursor=` and `?limit=` (default 100, max 1000) and answer
`{"items": [...], "next_cursor"?, "resync"?}`. Cursors are opaque (base64url of
`<epoch>:<offset>`); a cursor whose epoch no longer matches (daemon restart, store
generation change) yields an empty page with `"resync": true`, and the client lists again
from the start instead of treating it as an empty delta. A malformed cursor is
`invalid_argument`.

## Operations

Long-running work is an operation: `{id, kind, target, state, stage?, progress{done,
total?, unit?}, created, updated, result?, error?}`. `target` is exact (a CID, path or
link), never something that could resolve differently later. States: `running`,
`succeeded`, `failed`, `cancelled`. Operations live in the daemon's memory (not across
restarts); finished ones are kept for up to an hour, at most 1000.

Events (`GET /operations/{id}/events?since=<seq>`, `application/x-ndjson`): one
`{seq, time, type, stage?, progress?, message?, operation?}` per line; `seq` starts at 1
and is the resume cursor. Types: `stage`, `progress`, `log`, `done`. The `done` event
carries the final operation and is always the last line. Progress reporters rate-limit
themselves (a few events per second).

`GET /operations/{id}/wait?timeout=30s` (max 5m per request; the client loops for longer
waits) returns the operation once finished, else a `timeout` error with the snapshot.
`POST /operations/{id}/cancel` cancels a running one. The CLI follows an operation with
`nu operations wait <id>` or by default after `nu add`/`nu pin add` (`--no-wait` prints
the operation instead); with `--json` it prints the NDJSON events.

## Routes (v0)

```text
GET    /api/v0/status                      daemon summary (version, pid, paths, operation counts)
GET    /api/v0/openapi.json                this API's description
GET    /api/v0/operations                  list (paged)
GET    /api/v0/operations/{id}
GET    /api/v0/operations/{id}/events      NDJSON stream
GET    /api/v0/operations/{id}/wait
POST   /api/v0/operations/{id}/cancel
POST   /api/v0/add        {path, recursive?, raw?, hash?, nocopy?, no_announce?, pin?} → 202 operation (result: AddResult)
GET    /api/v0/cat?target=                 bytes
GET    /api/v0/ls?target=                  listing
GET    /api/v0/pins                        list (paged)
POST   /api/v0/pins       {cid, name?}     → 202 operation (result: Pin)
DELETE /api/v0/pins/{cid}                  → 204
GET    /api/v0/dag?target=                 atproto-style JSON
POST   /api/v0/dag        JSON body        → {cid}
```

`add` passes a local path: API and daemon run as the same user on loopback, so the
daemon reads the file itself. Content routes answer `not_implemented` until the store
lands. GET never changes state.

## Loopback protections

The API accepts only requests whose `Host` is a loopback name or address (`localhost`,
`127.0.0.1`, `[::1]`) with this listener's port (DNS-rebinding defense). Requests other
than GET/HEAD are rejected with `forbidden` when they carry an `Origin` that is not the
API's own `http://<host>` or a `Sec-Fetch-Site` other than `same-origin`/`none`; the
gateway's origin is therefore rejected too. `OPTIONS` is refused and no `Access-Control-*`
header is ever sent, so a browser page cannot read responses or change state. Responses
carry `X-Content-Type-Options: nosniff` and `Cache-Control: no-store`. The full
[security contract](operations.md#api-gateway-and-public-exposure) precedes any
non-loopback exposure.

## Targets

`get`, `cat`, `ls` and `dag get` parse their argument with `node/target` per the
[Targets table](../spec.md#user-experience): `nu://`, `at://`, `rasl://` links; a `b…`
base32 CIDv1 with optional `/path`; `<handle-or-did>/<name>[/path]`; or
`<dataset>[@<version>][/path]`. Anything else is `invalid_argument` listing the forms.
Forms this build cannot act on are `not_supported` with an action.

## CLI

`nu [--json] [--nu-path <dir>] [--api <host:port>] <command> …`. `--json` is accepted
before or after the command and switches every command's stdout to JSON (errors stay on
stderr). The CLI uses only the `client` package (`node.Node` seam) to reach the daemon;
daemonless commands will plug an in-process node into the same seam.
