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

Provider-specific response IDs, model IDs, raw stop reasons, and
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

Runtime retries transient failures with bounded exponential backoff. It uses a
server delay as the minimum, respects cancellation and Deadline, and charges
every attempt to Provider call budgets. Retry is permitted only before the
first observable stream item. Once a delta or completed message is published,
the Run fails instead of risking duplicate output or Tool side effects

Each actual attempt emits `model.request.started` with `provider_call` and
`provider_attempt`. A retry emits `provider.retry.scheduled` with
`provider_retry.error.code`, the failed attempt, next attempt, server hint, and
effective delay. A terminal Provider failure adds `provider_error` to the
`run.failed` Event

These classifications follow the current [OpenAI API error guidance](https://developers.openai.com/api/docs/guides/error-codes),
[OpenAI rate-limit guidance](https://developers.openai.com/api/docs/guides/rate-limits),
and [Anthropic API error guidance](https://platform.claude.com/docs/en/api/errors)

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
Anthropic Messages, and ChatGPT Codex Responses. The Echo Provider applies the
text subset because it has no model transport, Tool generation, or token Usage

The first-party applications are executable examples:

- [OpenAI contract tests](../provider/openai/contract_test.go)
- [Anthropic contract tests](../provider/anthropic/contract_test.go)
- [ChatGPT Codex contract tests](../provider/openaicodex/contract_test.go)
- [Echo text contract test](../provider/echo/contract_test.go)

Protocol-specific request encoding and optional fields still need focused
adapter tests. The common suite fixes the observable provider-neutral boundary,
not every upstream API field
