package chatauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	providerbase "github.com/qed-runtime/qed/provider"
)

const (
	defaultIssuer          = "https://auth.openai.com"
	defaultClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultOAuthOriginator = "qed"
	maximumOAuthBodyBytes  = 1 << 20
)

type exchangedTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type refreshedTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type oauthClient struct {
	issuer   string
	clientID string
	client   providerbase.HTTPClient
}

func (client oauthClient) exchange(
	ctx context.Context,
	code, redirectURI, verifier string,
) (exchangedTokens, error) {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {client.clientID},
		"code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(client.issuer, "/")+"/oauth/token",
		strings.NewReader(values.Encode()),
	)
	if err != nil {
		return exchangedTokens{}, fmt.Errorf("create ChatGPT token exchange request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "qed-runtime")
	var tokens exchangedTokens
	if err := client.doJSON(request, &tokens); err != nil {
		return exchangedTokens{}, fmt.Errorf("exchange ChatGPT authorization code: %w", err)
	}
	if tokens.IDToken == "" || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return exchangedTokens{}, errors.New("ChatGPT token exchange returned incomplete credentials")
	}
	return tokens, nil
}

func (client oauthClient) refresh(ctx context.Context, refreshToken string) (refreshedTokens, error) {
	payload, err := json.Marshal(struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
	}{ClientID: client.clientID, GrantType: "refresh_token", RefreshToken: refreshToken})
	if err != nil {
		return refreshedTokens{}, fmt.Errorf("encode ChatGPT token refresh: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(client.issuer, "/")+"/oauth/token",
		bytes.NewReader(payload),
	)
	if err != nil {
		return refreshedTokens{}, fmt.Errorf("create ChatGPT token refresh request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "qed-runtime")
	var tokens refreshedTokens
	if err := client.doJSON(request, &tokens); err != nil {
		return refreshedTokens{}, fmt.Errorf("refresh ChatGPT authorization: %w", err)
	}
	if tokens.IDToken == "" && tokens.AccessToken == "" && tokens.RefreshToken == "" {
		return refreshedTokens{}, errors.New("ChatGPT token refresh returned no credentials")
	}
	return tokens, nil
}

func (client oauthClient) revoke(ctx context.Context, profile storedProfile) error {
	token := profile.RefreshToken
	tokenType := "refresh_token"
	clientID := client.clientID
	if token == "" {
		token = profile.AccessToken
		tokenType = "access_token"
		clientID = ""
	}
	if token == "" {
		return nil
	}
	payload, err := json.Marshal(struct {
		Token         string `json:"token"`
		TokenTypeHint string `json:"token_type_hint"`
		ClientID      string `json:"client_id,omitempty"`
	}{Token: token, TokenTypeHint: tokenType, ClientID: clientID})
	if err != nil {
		return fmt.Errorf("encode ChatGPT token revocation: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(client.issuer, "/")+"/oauth/revoke",
		bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("create ChatGPT token revocation request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "qed-runtime")
	response, err := client.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("send ChatGPT token revocation: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumOAuthBodyBytes))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("ChatGPT token revocation returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (client oauthClient) doJSON(request *http.Request, target any) error {
	response, err := client.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("send OAuth request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decodeOAuthError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maximumOAuthBodyBytes+1))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode OAuth response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode OAuth response: multiple JSON values")
		}
		return fmt.Errorf("decode OAuth response trailer: %w", err)
	}
	return nil
}

func (client oauthClient) httpClient() providerbase.HTTPClient {
	if client.client != nil {
		return client.client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func decodeOAuthError(response *http.Response) error {
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumOAuthBodyBytes+1))
	if readErr != nil {
		return fmt.Errorf("read OAuth HTTP %d response: %w", response.StatusCode, readErr)
	}
	var envelope struct {
		Error            any    `json:"error"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
		Code             string `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	code := envelope.Code
	switch value := envelope.Error.(type) {
	case string:
		if code == "" {
			code = value
		}
	case map[string]any:
		if code == "" {
			code, _ = value["code"].(string)
		}
		if envelope.Message == "" {
			envelope.Message, _ = value["message"].(string)
		}
	}
	message := envelope.ErrorDescription
	if message == "" {
		message = envelope.Message
	}
	code = safeOAuthText(code)
	message = safeOAuthText(message)
	result := fmt.Sprintf("OAuth endpoint returned HTTP %d", response.StatusCode)
	if code != "" {
		result += " " + code
	}
	if message != "" {
		result += ": " + message
	}
	return errors.New(result)
}

func safeOAuthText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}
