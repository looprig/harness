module github.com/looprig/harness

go 1.26.4

tool (
	github.com/securego/gosec/v2/cmd/gosec
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)

require (
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1
	github.com/google/go-tdx-guest v0.3.1
	github.com/looprig/fsstore v0.0.0-00010101000000-000000000000
	github.com/yuin/goldmark v1.8.2
	golang.org/x/crypto v0.52.0
	golang.org/x/net v0.55.0
)

require (
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.19.0 // indirect
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/anthropics/anthropic-sdk-go v1.46.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.2.0 // indirect
	github.com/ccojocar/zxcvbn-go v1.0.4 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/logger v1.1.1 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.14 // indirect
	github.com/googleapis/gax-go/v2 v2.22.0 // indirect
	github.com/gookit/color v1.6.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/invopop/jsonschema v0.13.0 // indirect
	github.com/looprig/core v0.0.0
	github.com/looprig/storage v0.0.0
	github.com/mailru/easyjson v0.9.2 // indirect
	github.com/openai/openai-go/v3 v3.37.0 // indirect
	github.com/securego/gosec/v2 v2.27.1 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/telemetry v0.0.0-20260508192327-42602be52be6 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/tools v0.45.0 // indirect
	golang.org/x/vuln v1.3.0 // indirect
	google.golang.org/api v0.274.0 // indirect
	google.golang.org/genai v1.58.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.81.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

// fsstore is a storekit filesystem backend, normally wired at the composition root
// (not by looprig). It is pulled in ONLY by the //go:build integration
// workspace suspend/resume e2e test, via this test-scoped local replace mirroring the
// storekit one above (fsstore has no remote; the replace resolves it locally).
replace github.com/looprig/fsstore => ../fsstore

replace github.com/looprig/core => ../core

replace github.com/looprig/storage => ../storage
