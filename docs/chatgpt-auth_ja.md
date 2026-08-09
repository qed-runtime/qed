# ChatGPT subscription認証

QEDは独自のRuntime、Tool loop、Extension、Session、Evidenceを維持したまま、ChatGPT subscriptionでChatGPT Codex backendを呼び出せます

この経路はOpenAI API keyとAPI課金を使う`openai-responses`とは別です
ChatGPT planはAPI keyにはならず、API keyは`openai-codex`の認証にはなりません

## Sign in

browser loginはPKCE付きOAuth authorization code flowとloopback callbackを使います
QEDはcodeを交換する前にcallback stateを検証します

```sh
qed auth login --auth-profile personal
```

browserを起動せずauthorization URLだけを表示する場合は`--no-open`を使います

headless環境ではdevice-code flowを使います

```sh
qed auth login --auth-profile server --device-code
```

ChatGPT account側でdevice-code loginを利用できる必要があります
QEDはverification URLと一度だけ使うuser codeを表示し、authorizationを待機します

profile名により、tokenをproject設定に入れずに複数のcredentialを独立して保持できます

```sh
qed auth status
qed auth status --auth-profile personal
qed auth logout --auth-profile personal
```

`status`が返すのはprofile名、期限、plan metadata、存在する場合のemailだけです
tokenとChatGPT account IDは表示しません
logoutはlocal profileを先に削除し、その後remote token revokeを試みます

## Modelの実行

利用可能なmodelはaccount、plan、現在のbackend catalogに依存するため、model identifierを明示します

```sh
qed run \
  --provider openai-codex \
  --auth-profile personal \
  --model "<codex-model-id>" \
  --prompt "Reply with a short greeting"
```

同等のProvider profileは次のとおりです

```json
{
  "protocol": "openai-codex",
  "model": "<codex-model-id>",
  "auth_profile": "personal"
}
```

`openai-codex`は固定のHTTPS backendを使います
ChatGPT credentialが設定済みendpointへ転送されないように`base_url`、`token_env`、`max_output_tokens`、`api_version`を拒否します

## Credential保存とrefresh

QEDはGoの`os.UserConfigDir`が返すdirectory配下の`qed/auth.json`へ独自の名前付きprofileを保存します
他applicationのcredential fileは読み書きしません

fileにはraw ID token、access token、refresh tokenが含まれます
Unixでは親directoryをmode `0700`で作成し、fileをmode `0600`でatomicに書き込み、symlinkまたは権限が広いfileを拒否します
複数のQED processはlocal lockを使って更新を調停します
このfileを長期credentialとして保護し、commit、project内への配置、公開をしないでください

QEDは期限直前にcredentialをrefreshします
backendがHTTP 401を返した場合、Providerは名前付きprofileをreloadまたはrefreshし、そのrequestを1回だけ再試行します
CLI statusとQEDのstructured diagnosticsにtoken valueは含まれません

## Protocolの状態

OAuthとbackend behaviorは現在のChatGPT Codex client contractに追従します
QEDは他clientを偽装せず、独自の`qed` originatorを送信します

model availabilityとChatGPT backend contractはpublic OpenAI APIとは独立して変更される可能性があるため、この統合はexperimentalです
現在のQEDはfull Codex ResponsesをSSEで送信します
backend model discovery、internal Responses Lite dialect、WebSocket transportは未実装です

上流のaccountとdevice-code behaviorはOpenAIの現在の[authentication documentation](https://learn.chatgpt.com/docs/auth)を参照してください
