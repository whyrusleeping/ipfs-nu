// Package atproto fetches atproto records and repository snapshots for Nu (D21 "Using
// atproto data", D6 "Resolution and caching"): it resolves handles and DIDs through the
// SSRF-guarded httpsrc client, reads the PDS endpoint and signing key from the DID
// document, and fetches com.atproto.sync.getRecord proofs, getRepo CARs and blobs, all
// verified with core/atrepo before anything is returned or stored.
//
// It is the only package that talks to atproto services. Nothing here is atproto
// infrastructure Nu runs: the PDS, the PLC directory and handle DNS belong to the account.
package atproto
