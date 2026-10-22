// Package pilot is the opt-in benchmark runner of backlog 0034 (docs/pilots.md): it runs
// the bench/fixtures workload shapes through a one-node archive and a three-member
// in-process cluster (nutest) and records add/update cost, transfer bytes and storage
// growth per version, deduplication, budget behaviour, restore time, repair after a
// member loss and the operator interventions each needed.
//
// The runner is TestPilot in this package and only runs when asked:
//
//	NU_BENCH_PILOT=all go test ./bench/pilot -run TestPilot -v -timeout 1h
//
// Environment:
//
//	NU_BENCH_PILOT     "all" or a comma-separated list of shapes (required)
//	NU_BENCH_SCALE     fixtures.Params.Scale (default 1: a few MiB to ~50 MiB per version)
//	NU_BENCH_VERSIONS  versions per shape (default 4)
//	NU_BENCH_SEED      fixture seed (default 1)
//	NU_BENCH_CLUSTER   "0" skips the three-member cluster part
//	NU_BENCH_OUT       directory for results.json and results.md (default: test log only)
//	TMPDIR             where stores and fixtures live (default tmpfs via nutest/testenv;
//	                   set it to a disk directory to include fsync cost)
//
// Results from synthetic fixtures on one machine are harness evidence, not pilot evidence
// and not support claims.
package pilot
