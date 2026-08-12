# Provider Adapter

QEDはmodel wire protocolをprovider-neutralな`agent.Provider` interfaceの背後へ置きます
Providerはrequest変換、transport streaming、response正規化、Provider固有continuation stateを所有します
RuntimeはAgent loop、Tool実行、Session、Budget、Eventを所有します

## Provider contract

Provider実装は次を満たす必要があります

- `Name`から安定した空でないidentityを返す
- 独立したRunから安全に並行利用できる
- `Stream`へ渡されたContextに従う
- 空でない`text.delta` Eventを順序どおりに出す
- assistant Messageを持つterminal `message.completed` Eventを正確に1回出す
- terminal Eventの後は安定して`io.EOF`を返す
- blocked `Next`と並行して`Close`でき、resourceを解放する
- Tool Call、stop reason、Usageを`agent` typeへ正規化する
- HTTP-based Providerではnon-success responseを`*provider.HTTPError`として返す

Provider固有response ID、model ID、raw stop reason、`ProviderState`はadapter所有fieldのままです
Runtimeは`ProviderState`をopaqueとして扱い、同じProvider identityだけへ返します

## Provider errorとretry

`provider.ClassifyError`はwrapされたtransport errorとAPI errorを1つの安定したcodeへ変換します
custom Provider errorは`provider.ClassifiedError`を実装することでmessage matchingなしに分類へ参加できます

| Code | automatic retry | 意味 |
| --- | --- | --- |
| `retryable` | yes | 一時的なnetwork、timeout、overload、server failure |
| `rate_limited` | yes | 一時的なrequestまたはtoken rate limit |
| `authentication` | no | credentialの欠落、無効、期限切れ、拒否 |
| `invalid_request` | no | request shape、size、parameterの変更が必要 |
| `terminal` | no | permission、billing、quota、conflict、unknown、その他の永続failure |

HTTP Adapterは有効な`Retry-After` headerをparseした`RetryAfter` durationを含む`*provider.HTTPError`を返します
accepted streamが後からstructured API errorを出した場合はwrapされた`*provider.APIError`を返します
unknown errorはterminalです
特にbillingとquotaのerror codeはHTTP statusが429でもterminalのままです

Runtimeはtransient failureをbounded exponential backoffと小さなbounded per-Run jitterでretryします
server delayを最小値として使い、cancelとDeadlineに従い、すべてのattemptをProvider call budgetへ計上します
retryは最初の観測可能なstream itemより前だけに許可されます
deltaまたはcompleted messageを公開した後はoutputやTool副作用の重複を避けるためRunを失敗させます

各actual attemptは`provider_call`と`provider_attempt`を持つ`model.request.started`を出します
retryはfailure code、失敗attempt、次attempt、server hint、実効delayを`provider_retry`に持つ`provider.retry.scheduled`を出します
terminalなProvider failureは`run.failed` Eventへ`provider_error`を追加します

分類は現在の[OpenAI API error guidance](https://developers.openai.com/api/docs/guides/error-codes)、
[OpenAI rate-limit guidance](https://developers.openai.com/api/docs/guides/rate-limits)、
[Anthropic API error guidance](https://platform.claude.com/docs/en/api/errors)に基づきます

## Provider rate制御

各Runtimeはactive streamを既定で4つに制限する`ProviderRateLimiter`を持ちます
limiter permitは`Provider.Stream`が戻るまでではなく、Provider streamをconsumeしてcloseするまで保持されます
宣言設定はProvider profileごとに1つのlimiterを作り、そのprofileを参照する全Agentで共有します
直接APIを利用する場合は`NewProviderRateLimiter`で作成し、同じpointerを`agent.Options.ProviderRateLimiter`へ渡せます

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

別のlocal実装またはdistributed実装が必要な組み込み先は公開`ProviderRateLimitController` contractを注入できます

`rate_limited` failureが発生すると、実効retry delayをprofile共有cooldownにも反映します
有効な`Retry-After`を最小値とし、存在しない場合はretry policyのexponential fallbackを使います
これにより、1つ目のRunが観測した制限へ2つ目のRunが即座にrequestを送ることを防ぎます

active capacityまたはcooldownの待機では`provider.rate_limit.waiting`を出します
`provider_rate_limit_wait` fieldには本文を含まない`reason`、`max_concurrency`、利用可能な場合は残りの`retry_after_ms`が入ります
`reason`は`concurrency`または`cooldown`です
待機中のRunはcancelとDeadlineに従います
permit取得まではRuntime localと共有Provider call budgetを消費せず、後続の`model.request.started`が実際に計上したattemptを識別します

このlimiterはlocalな安全上限であり、RPM、input token、output tokenの完全なschedulerではありません
endpointとmodelの文字列だけではupstreamの共有bucketを推測できないため、別profileは意図的に独立させます
この挙動は現在の[OpenAI rate-limit guidance](https://developers.openai.com/api/docs/guides/rate-limits)と
[Anthropic rate-limit guidance](https://platform.claude.com/docs/en/api/rate-limits)に基づきます

## Contract test kit

`provider/contracttest`は公開された再利用可能なtest packageです
完全版`Run` suiteはstreaming HTTP Providerを対象に、正常stream lifecycle、2つのtext delta、1つのTool Call、正規化Usage、HTTP error mapping、stream内error、Context cancellation、並行`Close`を検証します

factoryはScenarioごとに決定的なscripted transportを持つ新しいProviderを構築する必要があります
実API、実credential、外部networkへ依存させません

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
			// protocol固有fieldをここで検証する
		},
	})
}
```

`FixtureRequest`はsuiteが使うprovider-neutral requestを返します
exportされたfixture constantとvalue functionがscripted responseを定義します

| Scenario | 必須のscripted behavior |
| --- | --- |
| `ScenarioText` | `FixtureTextDeltas()`を出し、`FixtureText`でcompleteする |
| `ScenarioToolCall` | `FixtureToolCallID`、`FixtureToolName`、`FixtureToolArguments()`でcompleteする |
| `ScenarioUsage` | textとTool Callを含めず`FixtureUsage()`でcompleteする |
| `ScenarioHTTPError` | exportされたHTTP status、error field、request IDを`*provider.HTTPError`として返す |
| `ScenarioStreamError` | startup成功後、`FixtureStreamErrorMessage`を含むerrorを返す |
| `ScenarioCancellation` | request Contextがcancelされるまで`Next`をblockし、`context.Canceled`を返す |
| `ScenarioClose` | `Close`が中断するまで`Next`をblockする |

`ResponseID`、`Model`、`RawStopReason`、opaqueな`ProviderState`など共通contract外のfieldには`AssertMessage`を使います
protocol固有error detailには`AssertError`を使います
これらのassertionを共通suiteへ重複させません

## 機能限定Provider

決定的なlocal Providerはmodel生成Tool Callやtoken Usageを持たない場合があります
完全conformanceを主張せず同じ正常stream lifecycleを適用するには`RunText`を使います

```go
contracttest.RunText(t, contracttest.TextOptions{
	Provider:       echo.New(),
	Request:        request,
	ExpectedText:   "hello",
	ExpectedDeltas: []string{"hello"},
})
```

QEDはOpenAI Responses、OpenAI Chat Completions、Anthropic Messages、ChatGPT Codex Responsesへ完全suiteを適用します
Echo Providerはmodel transport、Tool生成、token Usageを持たないためtext subsetを適用します

first-partyの適用箇所は実行可能なexampleです

- [OpenAI contract test](../provider/openai/contract_test.go)
- [Anthropic contract test](../provider/anthropic/contract_test.go)
- [ChatGPT Codex contract test](../provider/openaicodex/contract_test.go)
- [Echo text contract test](../provider/echo/contract_test.go)

protocol固有request encodingとoptional fieldには引き続きadapter固有testが必要です
共通suiteが固定するのは観測可能なprovider-neutral境界であり、upstream APIのすべてのfieldではありません
