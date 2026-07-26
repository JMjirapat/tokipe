module github.com/JMjirapat/tokipe/metrics/otel

// Pinned to 1.23 to match the minimum this project documents and its CI matrix
// tests. `go mod tidy` will happily raise both this directive and the OTel
// version if allowed to pick the latest; OTel >= 1.40 requires Go 1.25, which
// would silently make this module unbuildable for a supported Go 1.23 user.
// Verify with: GOTOOLCHAIN=local go build ./...
go 1.23

require (
	github.com/JMjirapat/tokipe v1.0.0
	go.opentelemetry.io/otel v1.31.0
	go.opentelemetry.io/otel/metric v1.31.0
	go.opentelemetry.io/otel/sdk/metric v1.31.0
)

require (
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/otel/sdk v1.31.0 // indirect
	go.opentelemetry.io/otel/trace v1.31.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
)

replace github.com/JMjirapat/tokipe => ../..
