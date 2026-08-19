# Provider adapters

QED keeps model wire protocols behind the provider-neutral `agent.Provider`
interface. A Provider owns request conversion, transport streaming, response
normalization, and Provider-private continuation state. Runtime owns the Agent
loop, Tool execution, Sessions, Budget, and Events

## Provider contract

A Provider implementation must:

- return a stable, non-empty identity from `Name`
- be safe for concurrent use by independent Runs
- honor the Context passed to `Stream`
- emit non-empty `text.delta` events in order
- emit exactly one terminal `message.completed` event with an assistant Message
- return stable `io.EOF` after the terminal event
- allow `Close` concurrently with a blocked `Next` and release its resources
- normalize Tool Calls, stop reasons, and Usage into `agent` types
- for an HTTP-based Provider, return `*provider.HTTPError` for a non-success response

Provider-specific request IDs, response IDs, model IDs, raw stop reasons, and
`ProviderState` remain adapter-owned fields. Runtime treats `ProviderState` as
opaque and only sends it back to the same Provider identity

## Provider errors and retry

`provider.ClassifyError` maps wrapped transport and API errors to one stable
code. A custom Provider error can implement `provider.ClassifiedError` to
participate without relying on message matching

| Code | Automatically retryable | Meaning |
| --- | --- | --- |
| `retryable` | yes | Temporary network, timeout, overloaded, or server failure |
| `rate_limited` | yes | Temporary request or token rate limit |
| `authentication` | no | Missing, invalid, expired, or rejected credential |
| `invalid_request` | no | Request shape, size, or parameter must change |
| `terminal` | no | Permission, billing, quota, conflict, unknown, or other permanent failure |

HTTP Adapters return `*provider.HTTPError`, including a parsed `RetryAfter`
duration when the response has a valid `Retry-After` header. An accepted stream
that later emits a structured API error returns a wrapped `*provider.APIError`
Unknown errors are terminal. In particular, billing and quota error codes stay
terminal even when their HTTP status is 429

Runtime retries transient failures with bounded exponential backoff and small
bounded per-Run jitter. It uses a server delay as the minimum, respects
cancellation and Deadline, and charges every attempt to Provider call budgets.
Retry is permitted only before the first observable stream item. Once a delta
or completed message is published, the Run fails instead of risking duplicate
output or Tool side effects

Each actual attempt emits `model.request.started` with `provider_call` and
`provider_attempt`. A retry emits `provider.retry.scheduled` with
`provider_retry.error.code`, the failed attempt, next attempt, server hint, and
effective delay. A terminal Provider failure adds `provider_error` to the
`run.failed` Event

These classifications follow the current [OpenAI API error guidance](https://developers.openai.com/api/docs/guides/error-codes),
[OpenAI rate-limit guidance](https://developers.openai.com/api/docs/guides/rate-limits),
and [Anthropic API error guidance](https://platform.claude.com/docs/en/api/errors)

## Provider rate control

Every Runtime has a `ProviderRateLimiter` that defaults to four active streams.
The limiter permit is held until the Provider stream is consumed and closed,
not only until `Provider.Stream` returns. Declarative configuration creates one
limiter per Provider profile and shares it between all Agents that reference
that profile. Direct API users can create one with `NewProviderRateLimiter` and
pass the same pointer through `agent.Options.ProviderRateLimiter`

```go
limiter, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{
    MaxConcurrency: 2,
})
if err != nil {
    return err
}

first, err := agent.NewRuntime(agent.Options{
    Provider:            firstProvider,
    ProviderRateLimiter: limiter,
})
if err != nil {
    return err
}
// Construct every Runtime in the same upstream pool with limiter
```

Embedders that need a different local or distributed implementation can supply
the public `ProviderRateLimitController` contract

When a `rate_limited` failure occurs, the effective retry delay also becomes a
shared profile cooldown. A valid `Retry-After` is the minimum; otherwise the
retry policy's exponential fallback applies. This keeps a second Run from
immediately sending into a limit already observed by the first Run

Waiting for active capacity or cooldown emits
`provider.rate_limit.waiting`. Its `provider_rate_limit_wait` field contains a
content-free `reason` (`concurrency` or `cooldown`), `max_concurrency`, and the
remaining `retry_after_ms` when available. A waiting Run respects cancellation
and Deadline. It consumes no Runtime-local or shared Provider call budget until
the permit is acquired; the following `model.request.started` identifies the
actual charged attempt

The limiter is a local safety bound rather than a complete RPM, input-token, or
output-token scheduler. Separate profiles intentionally remain independent
because upstream shared buckets cannot be inferred from endpoint and model
strings alone. This behavior follows the current [OpenAI rate-limit guidance](https://developers.openai.com/api/docs/guides/rate-limits)
and [Anthropic rate-limit guidance](https://platform.claude.com/docs/en/api/rate-limits)

## OrcaRouter Adapter

`provider/orcarouter` reuses the tested OpenAI Responses and Chat Completions
wire codecs while owning OrcaRouter routing behavior. The public APIs are
`APIResponses` and `APIChatCompletions`; the corresponding CLI and JSON protocol
names are `orcarouter-responses` and `orcarouter-chat`

The Adapter sets `X-OrcaRouter-Session-Id` on each call. A configured QED
Session takes priority, while an ephemeral Run uses its Run ID so every Tool
turn in that Run remains together. The header value is a domain-separated
SHA-256 digest over Provider, configured model, Agent, and scope identities
rather than the raw QED identifier. This is stable routing input, not a secret or authorization
token

It also sets `X-OrcaRouter-Include-Cost: true` and normalizes:

- `X-Orca-Request-Id` to `agent.Message.RequestID` on success and `provider.HTTPError.RequestID` on HTTP errors
- `X-Orca-Resolved-Model` to `agent.Message.Model` when present
- `usage.cost_usd` to rounded integer `agent.Usage.CostMicros`

These fields follow OrcaRouter's current [Session Affinity](https://docs.orcarouter.ai/routing/session-affinity),
[response header](https://docs.orcarouter.ai/routing/response-headers), and
[per-request cost](https://docs.orcarouter.ai/api-reference/chat/create-a-chat-completion#headers) contracts

Routed identifiers may resolve to models with different prompt-cache request
fields and reporting behavior. The Adapter therefore reports no QED cache
capability by default. A host may declare `CacheCapabilities` only after
verifying the selected model or router contract. OrcaRouter Session Affinity
and upstream implicit caching remain independent of that QED Cache Plan

## Contract test kit

`provider/contracttest` is a public reusable test package. Its complete `Run`
suite targets streaming HTTP Providers and checks successful stream lifecycle,
two text deltas, one Tool Call, normalized Usage, HTTP error mapping, an
in-stream error, Context cancellation, and a concurrent `Close`

The factory must construct a fresh Provider with a deterministic scripted
transport for every Scenario. It must not call a live API, read a real
credential, or depend on external network access

```go
func TestProviderContract(t *testing.T) {
	contracttest.Run(t, contracttest.SuiteOptions{
		NewProvider: func(
			t *testing.T,
			scenario contracttest.Scenario,
		) agent.Provider {
			return newScriptedProvider(t, scenario)
		},
		AssertMessage: func(
			t *testing.T,
			scenario contracttest.Scenario,
			message agent.Message,
		) {
			// Check protocol-specific fields here
		},
	})
}
```

`FixtureRequest` returns the provider-neutral request used by the suite. The
exported fixture constants and value functions define the scripted response:

| Scenario | Required scripted behavior |
| --- | --- |
| `ScenarioText` | Emit `FixtureTextDeltas()` and complete with `FixtureText` |
| `ScenarioToolCall` | Complete with `FixtureToolCallID`, `FixtureToolName`, and `FixtureToolArguments()` |
| `ScenarioUsage` | Complete without text or a Tool Call and with `FixtureUsage()` |
| `ScenarioHTTPError` | Return the exported HTTP status, error fields, and request ID as `*provider.HTTPError` |
| `ScenarioStreamError` | Start successfully, then return an error containing `FixtureStreamErrorMessage` |
| `ScenarioCancellation` | Block `Next` until the request Context is canceled, then return `context.Canceled` |
| `ScenarioClose` | Block `Next` until `Close` interrupts it |

Use `AssertMessage` for fields that are intentionally outside the common
contract, such as `ResponseID`, `Model`, `RawStopReason`, and opaque
`ProviderState`. Use `AssertError` for additional protocol-specific error
details. Do not duplicate those assertions in the common suite

## Capability-limited Providers

A deterministic local Provider may not produce model-generated Tool Calls or
token Usage. Use `RunText` to apply the same successful stream lifecycle checks
without claiming complete conformance

```go
contracttest.RunText(t, contracttest.TextOptions{
	Provider:       echo.New(),
	Request:        request,
	ExpectedText:   "hello",
	ExpectedDeltas: []string{"hello"},
})
```

QED applies the complete suite to OpenAI Responses, OpenAI Chat Completions,
OrcaRouter Responses and Chat Completions, Anthropic Messages, and ChatGPT Codex Responses. The Echo Provider applies the
text subset because it has no model transport, Tool generation, or token Usage

The first-party applications are executable examples:

- [OpenAI contract tests](../provider/openai/contract_test.go)
- [OrcaRouter contract tests](../provider/orcarouter/contract_test.go)
- [Anthropic contract tests](../provider/anthropic/contract_test.go)
- [ChatGPT Codex contract tests](../provider/openaicodex/contract_test.go)
- [Echo text contract test](../provider/echo/contract_test.go)

Protocol-specific request encoding and optional fields still need focused
adapter tests. The common suite fixes the observable provider-neutral boundary,
not every upstream API field
