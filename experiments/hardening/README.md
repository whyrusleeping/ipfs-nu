# Post-v0 hardening (0068)

Independent format oracles live in the experiments module; they add **no product
module dependencies**. Everything here is local/offline after fetching dependencies.
Results and limitations: [validation](../../docs/validation.md#post-v0-hardening-0068-2026-10-21).

From the repository root:

```sh
# Every main-module fuzz target, discovered from Go's test listing. Nonzero if any
# target fails, including setup failures/timeouts; all other targets still run.
python3 scripts/fuzz.py --time 60s --jobs 4
# Review and commit Go's minimized <package>/testdata/fuzz/<FuzzTarget>/* crashers.
# No crashers were produced in the recorded first campaign.

(cd experiments && go test -count=1 -v ./hardening)
(cd experiments && go test -count=1 -run '^TestIndigo' -v .)

# Rebuild the independent Rust oracle, using the committed lockfile. Compare, do
# not silently replace the golden results when upgrading either implementation.
cargo run --locked --release --manifest-path experiments/bao-vectors/Cargo.toml > /tmp/bao-tree-16k.json
diff -u testdata/vectors/bao/bao-tree-16k.json /tmp/bao-tree-16k.json
go test ./core/blob

NU_SLOW=1 GOMAXPROCS=4 go test -run '^$' -bench '^BenchmarkHardening$' -benchtime=1s -count=3 ./core/blob
NU_SLOW=1 GOMAXPROCS=4 go test -count=3 -run '^TestHardeningFanout$' -v ./core/tree
NU_SLOW=1 GOMAXPROCS=4 go test -count=1 -run '^TestHardeningCrashCampaign$' -v ./store
# Needs 35 GB free; source/store/output files are TempDir-managed, not retained.
NU_SLOW=1 GOMAXPROCS=4 go test -timeout=30m -count=1 -run '^TestHardening10GB$' -v ./nutest
```

`NU_MST_TREE=/path/to/checkout` replaces the Go source checkout used by the fanout
measurement. It preserves directory/name distributions but assigns fixed-size file
entry values (not real file payloads); no checkout is modified. The other inputs are
1000 node_modules-like directories with 100 names each and one 1M-entry flat directory.
Cold lookup means a new MST handle over a memory block store, not cold disk caches.

The pinned upstream DASL suite contains CBOR/CID fixtures, **no CAR fixture files**.
`TestDASLMatrix` accounts for every fixture, selecting basic/dag-cbor/dasl-cid tags
and reporting other-profile cases as inapplicable. CAR v1 and SHA-256/BLAKE3 payloads
are separately cross-checked with go-car. Strict Nu rejection is tested against the
fixture expectations, not inferred from a potentially permissive independent decoder.
The one precisely allowlisted oracle divergence is prime v0.24.0 turning CBOR
`3bffffffffffffffff` (-(2^64)) into zero; Nu must preserve the original bytes.

The Rust tool uses bao-tree **0.16.1**, `BlockSize::from_chunk_log(4)`, data byte
`i % 251`, and pre-order outboards. It adds the eight-byte little-endian size header
that Nu uses; bao-tree's range encoder omits that header. Vectors contain root hashes,
BLAKE3 digests of the complete outboard/slice bytes, and slice lengths. This covers
byte parity without checking in megabytes of repeated payload. It is a build-time
oracle only, not an iroh dependency in Nu.

`.github/workflows/hardening.yml` runs these campaigns nightly/on demand, with native
macOS arm64/amd64 and Linux arm64 jobs. Local output is under ignored `.hardening/`;
CI uploads fuzz logs/seeds and scale/benchmark logs even after failures. Slow work is
never in the per-PR test suite. Native CI execution is not yet observed.
