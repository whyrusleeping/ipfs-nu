# Nu

Nu is a Go daemon and CLI that brings back the magic of early IPFS: add files, share a
link, fetch them on another machine and open them in a browser. The same product also
handles versioned datasets, verified storage and replication across a cluster, so when
your data starts to matter you don't need a second stack.

- **Content-addressed, verified data.** The data model is [DASL](https://dasl.ing): SHA-256
  DRISL for structure, BLAKE3 for file blobs, with verified ranges and resumable
  multi-peer downloads.
- **libp2p over QUIC.** Nu has its own focused libp2p implementation that is
  wire-compatible with deployed go-, rust- and js-libp2p and the IPFS Amino DHT. It uses
  QUIC only, so UDP is required.
- **atproto for identity.** Existing atproto accounts (handles and DIDs) publish signed
  records that map stable public names to content, a better IPNS. Content commands need
  no account.

> **Status: early development.** Nothing has been released yet. The CLI, API, wire
> protocols and on-disk formats may still change without migration, and `nu` is a
> provisional name (it clashes with Nushell). Don't rely on it for data you can't replace.

## Build

You need Go 1.26 or later. Builds are pure Go (`CGO_ENABLED=0`) and run on Linux and
macOS.

```sh
git clone https://github.com/whyrusleeping/ipfs-nu
cd ipfs-nu
go build -o nu ./cmd/nu
```

The Go module path is `github.com/whyrusleeping/nu`, so build from a clone rather than with
`go install`. To install the binary along with a systemd/launchd service, use
[`dist/install.sh`](dist/install.sh) (`--user` by default, `--system` for Linux servers).

## Quick start

```sh
nu add -r ./my-site          # prints the tree CID and a nu:// share link
nu share <cid>               # serves the content until Ctrl-C (or run `nu daemon`)

# On another machine:
nu get 'nu://…'              # works with no daemon running
nu ls <cid>
nu cat <cid>/index.html
```

`nu daemon` runs the node in the background: libp2p on UDP `:4171`, the local API on
`127.0.0.1:5171` and a loopback gateway on `127.0.0.1:8171`, where you can browse
content at `http://127.0.0.1:8171/tree/<cid>/`. Any command creates `$NU_PATH`
(default `~/.nu`) the first time it runs. When no daemon is running, `get`, `cat`, `ls`
and `share` start an in-process node just for that command.

Other things to try:

```sh
nu status                    # daemon, identity, addresses, peers
nu doctor                    # connectivity checks in plain language
nu pin add|rm|ls <cid>
nu car export <cid>          # DASL CAR export/import
nu get at://<did>            # verified atproto repo snapshot; browse it with ls/cat
nu import ipfs <cid>         # one-way verified import from IPFS (Bitswap / trustless gateways)
nu login alice.example.com   # then: nu publish site <cid>; nu get alice.example.com/site
nu dataset create <ds>       # versioned one-node datasets (see versions, diff)
nu cluster init|invite|join  # replicated storage across trusted machines
```

Every command accepts `--json`, and `nu <command> --help` lists the flags. The CLI is
built on the [`client`](client) package over the local HTTP API, so anything the CLI can
do, a program can do too.

## Documentation

The design lives in [spec.md](spec.md), which covers goals, the CLI and API surface,
architecture, package layout and milestones, and links to detailed documents:

| | |
|---|---|
| [docs/product.md](docs/product.md) | Audience and user journeys |
| [docs/api.md](docs/api.md) | Local API, CLI conventions, error codes |
| [docs/data.md](docs/data.md) | CIDs, blobs, trees, transfer, pins and GC |
| [docs/dasl.md](docs/dasl.md) | DASL/atproto data, CAR, RASL, MASL |
| [docs/networking.md](docs/networking.md) | Wire profile, relays, NAT traversal, discovery |
| [docs/names.md](docs/names.md) | atproto identity and public names |
| [docs/follow.md](docs/follow.md) | Following and mirroring datasets |
| [docs/cluster.md](docs/cluster.md) | Clusters, replication and history |
| [docs/collections.md](docs/collections.md) | Growing item collections and the push API |
| [docs/migration.md](docs/migration.md) | Migrating from IPFS |
| [docs/operations.md](docs/operations.md) | Readiness, security, backup and recovery |
| [docs/validation.md](docs/validation.md) | Measurements and release evidence |

Planned and in-progress work is tracked in [backlog/](backlog).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build conventions, dependency rules and the
testing approach. `go test ./...` needs neither root nor network access and runs in about
a minute.

## License

MIT; see [LICENSE](LICENSE). Code adapted from other projects keeps its original license,
as listed in [NOTICE](NOTICE).
