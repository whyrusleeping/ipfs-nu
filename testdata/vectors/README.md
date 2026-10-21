# Test vectors

Byte-format vectors are permanent contracts (CIDs, DRISL, MST nodes, Bao, wire messages).
One directory per format, e.g. `cid/`, `drisl/`, `mst/`, `bao/`, `wire/`. Vendored
third-party suites keep their upstream file names plus a `SOURCE` file naming the upstream
repository and commit. Tests locate this directory relative to the package
(`../../testdata/vectors/...`).
