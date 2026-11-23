module github.com/hblabs-co/kit/web

go 1.26.2

require (
	github.com/expr-lang/expr v1.17.8
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/matoous/go-nanoid/v2 v2.1.0 // indirect
	github.com/oklog/ulid/v2 v2.1.1 // indirect
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/metric v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/sdk/metric v1.43.0
	go.opentelemetry.io/otel/trace v1.43.0
	golang.org/x/image v0.39.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.36.0 // indirect
)

require github.com/hblabs-co/kit/core v0.3.0

replace github.com/hblabs-co/kit/core => ../core
