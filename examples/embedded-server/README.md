# Embedded server

This example embeds a declaratively configured QED Host in a standard-library
HTTP server. It also links an application-owned Extension into the same binary
through an application-owned `extensions.lock`

The example intentionally has no authentication. Keep the default loopback
listener for local evaluation, or add the authentication, authorization, rate
limiting, request limits, and tenant isolation required by the containing
system before exposing it to a network

## Generate the application catalog

From the repository root

```sh
go run ./cmd/qed-extension-gen \
  --lock examples/embedded-server/extensions.lock \
  --output examples/embedded-server/extensionregistry/registry_gen.go \
  --package extensionregistry \
  --variable Catalog
```

An independent module that already requires QED can use the same tool path in
`go:generate`

```go
//go:generate go run github.com/qed-runtime/qed/cmd/qed-extension-gen --lock ../extensions.lock --output registry_gen.go --package extensionregistry --variable Catalog
```

Generation reads the lock and writes Go source only. Dependency versions and
checksums remain in the application's `go.mod` and `go.sum`

## Build and run

```sh
go build -o embedded-server ./examples/embedded-server
./embedded-server \
  --config ./examples/embedded-server/qed.json \
  --workspace . \
  --listen 127.0.0.1:8080
```

Run one request

```sh
curl \
  --request POST \
  --header 'Content-Type: application/json' \
  --data '{"prompt":"hello"}' \
  http://127.0.0.1:8080/runs
```

The executable dispatches `__extension` before parsing application flags. In
normal mode it loads one reusable `qed.Host`, shares it across requests, and
closes its Extension processes during shutdown. In child mode the same catalog
serves the selected Extension over Protocol v2

The example uses the deterministic echo Provider so it does not call the
`greet` Tool. Its integration test still starts and validates the linked
Extension process while exercising the HTTP handler. Replace the Provider
profile with a model Provider to expose the Tool to an actual model
