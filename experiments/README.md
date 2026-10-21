# experiments

A separate Go module for spikes and interop smoke tests. It may depend on go-libp2p, Kubo
or other reference implementations; the main module never imports it. Run its tests from
this directory (`cd experiments && go test ./...`); they are not part of the root suite.
