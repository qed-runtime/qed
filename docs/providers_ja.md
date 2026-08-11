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
