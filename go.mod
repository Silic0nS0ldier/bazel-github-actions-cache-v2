module github.com/cre4ture/bazel-github-actions-cache-v2

go 1.27.4

require (
	cloud.google.com/go/longrunning v1.5.0
	github.com/ipfs/go-block-format v0.2.4
	github.com/ipfs/go-cid v0.6.2
	github.com/ipld/go-car/v2 v2.17.0
	github.com/ipld/go-ipld-prime v0.24.0
	github.com/klauspost/compress v1.20.4
	github.com/multiformats/go-multihash v0.2.3
	github.com/tonistiigi/go-actions-cache v0.0.0-20260930145529-afe8013b5ac4
	golang.org/x/sync v0.25.0
	google.golang.org/genproto/googleapis/api v0.0.0-20261110000943-09e21b237f2c
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261110000943-09e21b237f2c
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	lukechampine.com/blake3 v1.4.1
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.24.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.13.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/storage/azblob v1.8.3 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/storage/internal v0.1.0 // indirect
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/dimchansky/utfbom v1.2.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/ipfs/boxo v0.43.0 // indirect
	github.com/ipfs/go-ipld-cbor v0.3.0 // indirect
	github.com/ipfs/go-ipld-format v0.6.4 // indirect
	github.com/klauspost/cpuid/v2 v2.4.2 // indirect
	github.com/minio/sha256-simd v1.0.1 // indirect
	github.com/mr-tron/base58 v1.3.0 // indirect
	github.com/multiformats/go-base32 v0.1.0 // indirect
	github.com/multiformats/go-base36 v0.2.0 // indirect
	github.com/multiformats/go-multibase v0.3.0 // indirect
	github.com/multiformats/go-multicodec v0.10.0 // indirect
	github.com/multiformats/go-varint v0.1.0 // indirect
	github.com/petar/GoLLRB v0.0.0-20210522233825-ae3b015fd3e9 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/polydawn/refmt v0.90.0 // indirect
	github.com/spaolacci/murmur3 v1.1.0 // indirect
	github.com/whyrusleeping/cbor v0.0.0-20171005072247-63513f603b11 // indirect
	github.com/whyrusleeping/cbor-gen v0.3.1 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/exp v0.0.0-20261113113407-e3970619a1a4 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/telemetry v0.0.0-20261010193011-f71dc92da45e // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/tools v0.52.1-0.20261106145633-d407941a339a // indirect
	golang.org/x/vuln v1.9.0 // indirect
	golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool (
	golang.org/x/tools/cmd/deadcode
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
