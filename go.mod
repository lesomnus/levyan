module github.com/lesomnus/levyan

go 1.27.0

toolchain go1.27.3

// The generators, as tools of this module.
//
// They are pinned here rather than by payday because a plugin is a Go program
// built from **this** module's graph -- so its version is this app's to choose,
// and `pd doctor` is what says when one is missing rather than `buf generate`
// failing over an executable nobody has heard of.
//
// `go mod tidy` fills in the requires. Nothing is pinned in this file on
// purpose: a template that carried version numbers would be a template that is
// out of date the week after it was written.
tool (
	github.com/lesomnus/payday/cmd/pd
	github.com/lesomnus/payday/cmd/protoc-gen-pd
	github.com/protobuf-orm/ent/cmd/ent
	github.com/protobuf-orm/protobuf-merge
	github.com/protobuf-orm/protoc-gen-orm-ent
	github.com/protobuf-orm/protoc-gen-orm-go
	github.com/protobuf-orm/protoc-gen-orm-service
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
)

require (
	github.com/lesomnus/levyan/api v0.0.0-00010101000000-000000000000
	github.com/lesomnus/mkot/otlp v0.0.0-20261017184153-a69c4c1f08ca
	github.com/lesomnus/otx v0.0.0-20261017184159-4cba7a26b155
	github.com/lesomnus/payday v0.0.0-20261031085636-0d342d896262
	github.com/lesomnus/protobuf-patch v0.0.0-20260907061053-07bbf2b842da
	github.com/lesomnus/roster v0.1.2-0.20261031100757-81df289602a7
	github.com/lesomnus/xli v0.0.0-20261009121920-4ce270c7448c
	github.com/lesomnus/xli/cfg v0.1.3
	github.com/protobuf-orm/ent v0.0.0-20261024205432-0dd7dadc81bc
	github.com/protobuf-orm/protobuf-orm v0.0.0-20261017212716-3430237a8656
	github.com/protobuf-orm/protoc-gen-orm-ent/runtime v0.0.0-20260906232518-fa1777a2628d
	github.com/stretchr/testify v1.12.1
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.47.0
	go.opentelemetry.io/otel/log v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/metric v1.47.0
	go.opentelemetry.io/proto/otlp v1.11.1
	golang.org/x/sync v0.24.0
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261019180522-ce908d29299b
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	ariga.io/atlas v1.3.0 // indirect
	cel.dev/cel-go v0.32.0 // indirect
	cel.dev/expr v0.25.2 // indirect
	connectrpc.com/connect v1.20.0 // indirect
	connectrpc.com/cors v0.1.0 // indirect
	connectrpc.com/vanguard v0.4.0 // indirect
	filippo.io/age v1.3.2 // indirect
	filippo.io/hpke v0.4.0 // indirect
	github.com/agext/levenshtein v1.2.3 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/apparentlymart/go-textseg/v17 v17.0.1 // indirect
	github.com/bmatcuk/doublestar v1.3.4 // indirect
	github.com/bufbuild/protocompile v0.14.2-0.20260716165721-bb5762d29672 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/ettle/strcase v0.2.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/inflect v1.0.0 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.31.0 // indirect
	github.com/hashicorp/hcl/v2 v2.24.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/lesomnus/flob v0.0.0-20261010120939-21f89e60609e // indirect
	github.com/lesomnus/mkot v0.0.0-20261013182918-46d8b05a79b3 // indirect
	github.com/lesomnus/mkot/mkotx v0.0.0-20261013182918-46d8b05a79b3 // indirect
	github.com/lesomnus/mkot/pretty v0.0.0-20261013182918-46d8b05a79b3 // indirect
	github.com/lesomnus/otx/otxgrpc v0.0.0-20261017184159-4cba7a26b155 // indirect
	github.com/lesomnus/pwr v0.0.0-20261015171245-2ec220d490d1 // indirect
	github.com/lesomnus/pwr/cel v0.0.0-20261015172325-de464a5ef1fa // indirect
	github.com/lesomnus/z v0.0.0-20260907061127-c3858eb05878 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.23 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/ncruces/go-sqlite3 v0.35.3 // indirect
	github.com/ncruces/go-sqlite3-wasm/v3 v3.2.35304 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/petermattis/goid v0.0.0-20260716134002-a9b348f0a2b9 // indirect
	github.com/protobuf-orm/protobuf-merge v0.0.0-20261009205601-8502fad9e678 // indirect
	github.com/protobuf-orm/protoc-gen-orm-ent v0.0.0-20261021213534-6068959c562d // indirect
	github.com/protobuf-orm/protoc-gen-orm-go v0.0.0-20261010065542-f9ac5b66df3e // indirect
	github.com/protobuf-orm/protoc-gen-orm-service v0.0.0-20261017215512-5234846ec2e4 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/tidwall/btree v1.8.1 // indirect
	github.com/zclconf/go-cty v1.19.0 // indirect
	github.com/zclconf/go-cty-yaml v1.2.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/bridges/otelslog v0.21.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.71.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc v0.23.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.23.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/log v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/term v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/lesomnus/levyan/api => ./api
