module github.com/whyrusleeping/nu/experiments/quicgo-cid

go 1.26.0

require (
	github.com/quic-go/quic-go v0.64.1
	github.com/whyrusleeping/nu v0.0.0
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.49.0
)

require (
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 // indirect
	github.com/ipfs/go-cid v0.6.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/minio/sha256-simd v1.0.1 // indirect
	github.com/mr-tron/base58 v1.3.0 // indirect
	github.com/multiformats/go-base32 v0.1.0 // indirect
	github.com/multiformats/go-base36 v0.2.0 // indirect
	github.com/multiformats/go-multiaddr v0.16.1 // indirect
	github.com/multiformats/go-multibase v0.3.0 // indirect
	github.com/multiformats/go-multihash v0.2.3 // indirect
	github.com/multiformats/go-varint v0.1.0 // indirect
	github.com/spaolacci/murmur3 v1.1.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20231110203233-9a3e6036ecaa // indirect
	lukechampine.com/blake3 v1.4.1 // indirect
)

// Patched quic-go (0001-add-Config.ConnectionIDTracer.patch applied to v0.64.1),
// created by ./setup.sh; gitignored. The main module stays on stock quic-go.
replace github.com/quic-go/quic-go => ./quic-go

replace github.com/whyrusleeping/nu => ../..
