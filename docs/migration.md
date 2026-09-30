# IPFS adoption and migration

[Spec overview](../spec.md) · [Product brief](product.md) · [Data](data.md) · [Operations](operations.md)

Nu preserves the IPFS-style workflow and selected libp2p wire protocols. It is initially
neither a Kubo RPC replacement nor a UnixFS/Bitswap content peer. Old IPFS gateways cannot
be assumed to retrieve a Nu CID. The planned bridge is **verified, one-way import** into
Nu, not a second native storage/replication model.

Migration follows the first useful previews. It must be available and demonstrated before
recommending that existing IPFS deployments move their production content to Nu. Reverse
migration/export to UnixFS, ongoing bidirectional synchronization and Kubo API emulation
are not initial migration promises. The standard Linux/macOS production distribution
includes the verified importer; minimal embedded builds may omit it and must advertise that
limitation. Optional build tags do not justify omitting the supported migration release.

## Two journeys

### Import existing content

Given an IPFS root, obtain original blocks from a local installation or the old network,
verify them under their original CIDs, traverse the supported UnixFS/DAG representation,
and materialize the file bytes/tree in Nu. A remote gateway is a transport source, not
verification authority. Verify original links/blocks before deriving new content.

Nu's whole-file BLAKE3/tree representation generally produces different roots. Record the
mapping from original IPFS root to resulting Nu root/version. Do not alias old identifiers
as if the underlying representations were identical.

### Move an installation

Inventory local Kubo pins and IPFS Cluster datasets, policies, labels and automation.
Estimate space, import recoverable content locally without unnecessary network transfer,
map policies with explicit unsupported cases, validate retrieval and Nu replica readiness,
then produce a cutover report. Existing scripts require an API/CLI transition, not a claim
of drop-in compatibility. Never remove or modify the source installation automatically.

## Import contract

- Dry run identifies supported object types, missing content, intended metadata handling,
  estimated disk/headroom and policy mappings. Inventory can be incomplete; say so.
- Interrupted imports resume from verified progress. Bound resources and report errors per
  root/dataset; one unsupported item need not discard completed imports.
- Preserve exact file bytes and representable names/metadata. Define symlink, permissions,
  filename normalization and unsupported DAG/UnixFS forms before shipping. Transformations
  require explicit reporting/choice, not silent loss.
- Report original root, new root/version, verifier outcome, importer/version, imported
  files/bytes, metadata transformations, missing content and applied policy.
- Store provenance in a content-addressed import manifest (including importer version and
  reconciliation report), referenced by the signed dataset version's `imported` field.
  Standalone imports return its CID; exports include it. Conversion does not preserve
  original CID identity; original-block verification and the conversion report establish the
  relationship. A signed dataset version authenticates the manifest reference; a standalone
  manifest CID alone offers content integrity, not authenticated authorship. Record it in a
  dataset to add that provenance. Canonical schema/validation remains design work, not
  another transfer protocol.
- Validation reads from a fresh Nu client, including selected historical versions when
  applicable, and waits for the configured replication policy before suggesting cutover.
- Source credentials/private keys are not copied as incidental metadata. Cluster authority
  and public identity migration require separate deliberate setup.

## Acceptance and open design

Test local pinset import, remote retrieval, directory and standalone files, unsupported
objects, corrupt original blocks, lost providers, interrupted conversion, metadata conflicts
and large objects. Show a resumable migration of a representative existing installation
with a repeatable reconciliation report and no source mutation.

Still to design: supported UnixFS/DAG profile, bridge wire transports and Bitswap limits,
local Kubo/Cluster inventory adapters, import/provenance manifest encoding, policy mapping,
checkpointing and CLI/API syntax. Reuse the existing store, byte/tree transfer and operational
contracts after conversion. No transport implementation choice is made by this product brief.
