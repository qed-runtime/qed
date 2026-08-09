package chatauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSaveListAndResolveProfile(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	service := testService(t, nil, now)
	tokens := testTokens(now.Add(time.Hour), "account-1", "access-1", "refresh-1")
	info, err := service.saveTokens(context.Background(), "personal", tokens)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "personal" || info.Email != "user@example.com" || info.Plan != "plus" {
		t.Fatalf("ProfileInfo = %#v", info)
	}
	fileInfo, err := os.Stat(service.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %04o", fileInfo.Mode().Perm())
	}
	profiles, err := service.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].ID != "personal" {
		t.Fatalf("Profiles() = %#v", profiles)
	}
	source, err := service.CredentialSource("personal")
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := source.Authorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authorization.AccessToken != tokens.AccessToken || authorization.AccountID != "account-1" || !authorization.FedRAMP {
		t.Fatalf("Authorization() = %#v", authorization)
	}
}

func TestCredentialSourceSerializesRotatingRefresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	refreshed := testTokens(now.Add(time.Hour), "account-1", "access-2", "refresh-2")
	var refreshCalls atomic.Int32
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/oauth/token" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("refresh request = %s %s", request.URL.Path, request.Header.Get("Content-Type"))
		}
		var body struct {
			GrantType    string `json:"grant_type"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body.GrantType != "refresh_token" || body.RefreshToken != "refresh-1" {
			t.Errorf("refresh body = %#v", body)
		}
		refreshCalls.Add(1)
		return jsonResponse(http.StatusOK, refreshedTokens{
			IDToken: refreshed.IDToken, AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken,
		}), nil
	})
	service := testService(t, client, now)
	initial := testTokens(now.Add(time.Minute), "account-1", "access-1", "refresh-1")
	if _, err := service.saveTokens(context.Background(), "personal", initial); err != nil {
		t.Fatal(err)
	}
	source, err := service.CredentialSource("personal")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	var wait sync.WaitGroup
	errorsByCaller := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			authorization, err := source.Authorization(context.Background())
			if err == nil && authorization.AccessToken != refreshed.AccessToken {
				err = errors.New("authorization did not use refreshed access token")
			}
			errorsByCaller <- err
		}()
	}
	wait.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls.Load())
	}
}

func TestRecoverUnauthorizedReloadsCredentialRefreshedByAnotherCaller(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	service := testService(t, httpClientFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected refresh request")
		return nil, nil
	}), now)
	initial := testTokens(now.Add(time.Hour), "account-1", "access-1", "refresh-1")
	if _, err := service.saveTokens(context.Background(), "personal", initial); err != nil {
		t.Fatal(err)
	}
	source, _ := service.CredentialSource("personal")
	updated := testTokens(now.Add(2*time.Hour), "account-1", "access-2", "refresh-2")
	if _, err := service.saveTokens(context.Background(), "personal", updated); err != nil {
		t.Fatal(err)
	}
	authorization, err := source.RecoverUnauthorized(context.Background(), authorizationFromProfile(storedProfile{
		AccessToken: "access-1", AccountID: "account-1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if authorization.AccessToken != updated.AccessToken {
		t.Fatalf("Authorization = %#v", authorization)
	}
}

func TestLogoutRemovesProfileAndRevokesRefreshToken(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var revoked atomic.Bool
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/oauth/revoke" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		var body struct {
			Token         string `json:"token"`
			TokenTypeHint string `json:"token_type_hint"`
			ClientID      string `json:"client_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body.Token != "refresh-1" || body.TokenTypeHint != "refresh_token" || body.ClientID != defaultClientID {
			t.Errorf("revoke body = %#v", body)
		}
		revoked.Store(true)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	service := testService(t, client, now)
	if _, err := service.saveTokens(context.Background(), "personal", testTokens(now.Add(time.Hour), "account-1", "access-1", "refresh-1")); err != nil {
		t.Fatal(err)
	}
	result, err := service.Logout(context.Background(), "personal")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Removed || result.RevocationError != nil || !revoked.Load() {
		t.Fatalf("Logout() = %#v, revoked=%v", result, revoked.Load())
	}
	if err := service.ValidateProfile(context.Background(), "personal"); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("ValidateProfile() error = %v", err)
	}
}

func TestBrowserLoginExchangesCallbackAndPersistsProfile(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tokens := testTokens(now.Add(time.Hour), "account-1", "access-1", "refresh-1")
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/oauth/token" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("exchange request = %s %s", request.URL.Path, request.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		if values.Get("code") != "authorization-code" || values.Get("code_verifier") == "" ||
			!strings.HasPrefix(values.Get("redirect_uri"), "http://localhost:") {
			t.Errorf("exchange form = %v", values)
		}
		return jsonResponse(http.StatusOK, tokens), nil
	})
	service := testService(t, client, now)
	service.callbackPorts = []int{0}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var presentErr error
	info, err := service.LoginBrowser(ctx, "personal", BrowserLoginOptions{
		PresentURL: func(value string) {
			parsed, parseErr := url.Parse(value)
			if parseErr != nil {
				presentErr = parseErr
				cancel()
				return
			}
			callback, parseErr := url.Parse(parsed.Query().Get("redirect_uri"))
			if parseErr != nil {
				presentErr = parseErr
				cancel()
				return
			}
			query := callback.Query()
			query.Set("code", "authorization-code")
			query.Set("state", parsed.Query().Get("state"))
			callback.RawQuery = query.Encode()
			response, requestErr := (&http.Client{Timeout: 3 * time.Second}).Get(callback.String())
			if requestErr != nil {
				presentErr = requestErr
				cancel()
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				presentErr = errors.New("browser callback did not return HTTP 200")
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if presentErr != nil {
		t.Fatal(presentErr)
	}
	if info.ID != "personal" || info.Email != "user@example.com" {
		t.Fatalf("ProfileInfo = %#v", info)
	}
	if err := service.ValidateProfile(context.Background(), "personal"); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceLoginPollsAndPersistsProfile(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tokens := testTokens(now.Add(time.Hour), "account-1", "access-1", "refresh-1")
	verifier := "device-verifier"
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	var polls atomic.Int32
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			return jsonResponse(http.StatusOK, map[string]string{
				"device_auth_id": "device-1", "user_code": "ABCD-EFGH", "interval": "1",
			}), nil
		case "/api/accounts/deviceauth/token":
			if polls.Add(1) == 1 {
				return jsonResponse(http.StatusForbidden, map[string]string{"error": "pending"}), nil
			}
			return jsonResponse(http.StatusOK, map[string]string{
				"authorization_code": "authorization-code", "code_challenge": challenge, "code_verifier": verifier,
			}), nil
		case "/oauth/token":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			values, err := url.ParseQuery(string(body))
			if err != nil {
				return nil, err
			}
			if values.Get("code") != "authorization-code" || values.Get("code_verifier") != verifier ||
				values.Get("redirect_uri") != "https://auth.test/deviceauth/callback" {
				t.Errorf("exchange form = %v", values)
			}
			return jsonResponse(http.StatusOK, tokens), nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	})
	service := testService(t, client, now)
	service.wait = func(context.Context, time.Duration) error { return nil }
	var presented DeviceCode
	info, err := service.LoginDevice(context.Background(), "server", DeviceLoginOptions{
		PresentCode: func(code DeviceCode) { presented = code },
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "server" || presented.UserCode != "ABCD-EFGH" ||
		presented.VerificationURL != "https://auth.test/codex/device" || polls.Load() != 2 {
		t.Fatalf("info/code/polls = %#v/%#v/%d", info, presented, polls.Load())
	}
	if err := service.ValidateProfile(context.Background(), "server"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizationURLMatchesCurrentCodexOAuthContract(t *testing.T) {
	t.Parallel()

	service := testService(t, nil, time.Now())
	value := service.authorizationURL("http://localhost:1455/auth/callback", "challenge", "state")
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for name, want := range map[string]string{
		"response_type":              "code",
		"client_id":                  defaultClientID,
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"scope":                      oauthScope,
		"code_challenge":             "challenge",
		"code_challenge_method":      "S256",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"state":                      "state",
		"originator":                 "qed",
	} {
		if got := query.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestCredentialStoreRejectsBroadPermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission check")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"profiles":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := newCredentialStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.profiles(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("profiles() error = %v", err)
	}
}

func testService(t *testing.T, client httpClientFunc, now time.Time) *Service {
	t.Helper()
	store, err := newCredentialStore(filepath.Join(t.TempDir(), "qed", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := newService(store, oauthClient{issuer: "https://auth.test", clientID: defaultClientID, client: client})
	service.now = func() time.Time { return now }
	return service
}

func testTokens(expiresAt time.Time, accountID, accessMarker, refreshToken string) exchangedTokens {
	identityClaims := map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":         accountID,
			"chatgpt_plan_type":          "plus",
			"chatgpt_account_is_fedramp": true,
		},
	}
	accessClaims := map[string]any{
		"exp": expiresAt.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
		"marker": accessMarker,
	}
	return exchangedTokens{
		IDToken: makeJWT(identityClaims), AccessToken: makeJWT(accessClaims), RefreshToken: refreshToken,
	}
}

func makeJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (client httpClientFunc) Do(request *http.Request) (*http.Response, error) {
	if client == nil {
		return nil, errors.New("unexpected HTTP request")
	}
	return client(request)
}

func jsonResponse(status int, value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(data))),
	}
}
