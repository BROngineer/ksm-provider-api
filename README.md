# ksm-provider-api

The contract between [k0rdent](https://github.com/K0rdent/kcm) and its state
management providers, plus the Go code both sides use to implement it.

A *state management provider* (an adapter) deploys the services of k0rdent
ServiceSets with a tool such as Sveltos, Argo CD or Flux. kcm calls the adapter
over HTTP to deploy or delete a ServiceSet and turns the answer into the
ServiceSet status. Adapters find kcm through a registry they keep alive with
heartbeats, and tell kcm when a ServiceSet needs attention with Kubernetes
Events.

## Documentation

| Document | For |
|---|---|
| [API contract](docs/api.md) | everyone: the provider and registry APIs, errors, authentication, notifications |
| [kcm integration guide](docs/kcm-integration.md) | kcm developers |
| [Adapter developer guide](docs/adapter-guide.md) | adapter authors |

The OpenAPI specs in [`api/provider`](api/provider/openapi.yaml) and
[`api/registry`](api/registry/openapi.yaml) are the source of truth for the wire
format.

Reference adapter: [`ksm-provider-sveltos`](https://github.com/brongineer/ksm-provider-sveltos)
(ProjectSveltos).

## Packages

| Package | Purpose | Used by |
|---|---|---|
| `api/provider`, `api/registry` | code generated from the OpenAPI specs | all |
| `provider/server` | serves the `Provider` interface an adapter implements | adapters |
| `provider/client` | calls adapters | kcm |
| `registry/server` | the registry store and register handler | kcm |
| `registry/client` | `Heartbeater`: registration and heartbeats | adapters |
| `notify` | state-change notifications (`StateChanged`) and the kcm-side watch helpers | adapters, kcm |
| `auth` | TokenReview authentication, result cache, token audiences | adapters, kcm |
| `httpserver` | TLS server with certificate reload, used for both APIs; plain-HTTP insecure mode for testing and demos | adapters, kcm |
| `problem` | RFC 9457 problem details shared by both APIs | adapters, kcm |
| `providertest` | fakes, test servers and the provider contract suite | tests |

## Development

Tools are pinned with [mise](https://mise.jdx.dev):

| Task | Does |
|---|---|
| `mise run api-gen` | regenerates `api/*/*.gen.go` from the OpenAPI specs |
| `mise run lint` | `go fmt`, `go vet`, `go fix`, `golangci-lint` |
| `mise run test` | `go test -race ./...` |
| `mise run tidy` | `go mod tidy` |
