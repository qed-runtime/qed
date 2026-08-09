package chatauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const oauthScope = "openid profile email offline_access api.connectors.read api.connectors.invoke"

// BrowserLoginOptions configures an interactive browser login
type BrowserLoginOptions struct {
	// PresentURL receives the complete authorization URL after the callback
	// listener is ready. It may print the URL and open a browser
	PresentURL func(string)
}

type browserLoginResult struct {
	profile ProfileInfo
	err     error
}

// LoginBrowser signs in one named profile with OAuth authorization code and PKCE
func (service *Service) LoginBrowser(
	ctx context.Context,
	profileID string,
	options BrowserLoginOptions,
) (ProfileInfo, error) {
	if ctx == nil {
		return ProfileInfo{}, errors.New("ChatGPT browser login context must not be nil")
	}
	if err := validateProfileID(profileID); err != nil {
		return ProfileInfo{}, err
	}
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return ProfileInfo{}, err
	}
	state, err := randomURLToken(32)
	if err != nil {
		return ProfileInfo{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	listener, port, err := service.listenCallback()
	if err != nil {
		return ProfileInfo{}, err
	}
	defer listener.Close()
	redirectURI := "http://localhost:" + strconv.Itoa(port) + "/auth/callback"
	authorizationURL := service.authorizationURL(redirectURI, challenge, state)

	results := make(chan browserLoginResult, 1)
	var finishOnce sync.Once
	finish := func(result browserLoginResult) {
		finishOnce.Do(func() { results <- result })
	}
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet || request.URL.Path != "/auth/callback" {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("X-Content-Type-Options", "nosniff")
			callbackState := request.URL.Query().Get("state")
			if subtle.ConstantTimeCompare([]byte(callbackState), []byte(state)) != 1 {
				http.Error(writer, "OAuth state mismatch", http.StatusBadRequest)
				return
			}
			if errorCode := safeOAuthText(request.URL.Query().Get("error")); errorCode != "" {
				description := safeOAuthText(request.URL.Query().Get("error_description"))
				loginErr := fmt.Errorf("ChatGPT sign-in failed: %s", errorCode)
				if description != "" {
					loginErr = fmt.Errorf("ChatGPT sign-in failed: %s: %s", errorCode, description)
				}
				http.Error(writer, "ChatGPT sign-in was not completed", http.StatusForbidden)
				finish(browserLoginResult{err: loginErr})
				return
			}
			code := request.URL.Query().Get("code")
			if code == "" {
				http.Error(writer, "Missing OAuth authorization code", http.StatusBadRequest)
				finish(browserLoginResult{err: errors.New("ChatGPT sign-in returned no authorization code")})
				return
			}
			tokens, exchangeErr := service.oauth.exchange(ctx, code, redirectURI, verifier)
			if exchangeErr != nil {
				http.Error(writer, "ChatGPT token exchange failed", http.StatusBadGateway)
				finish(browserLoginResult{err: exchangeErr})
				return
			}
			profile, saveErr := service.saveTokens(ctx, profileID, tokens)
			if saveErr != nil {
				http.Error(writer, "ChatGPT credentials could not be saved", http.StatusInternalServerError)
				finish(browserLoginResult{err: saveErr})
				return
			}
			_, _ = fmt.Fprintln(writer, "ChatGPT sign-in completed. You can return to QED")
			finish(browserLoginResult{profile: profile})
		}),
	}
	serveErrors := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErrors <- err
	}()
	if options.PresentURL != nil {
		options.PresentURL(authorizationURL)
	}

	var result browserLoginResult
	select {
	case <-ctx.Done():
		result.err = ctx.Err()
	case result = <-results:
	case serveErr := <-serveErrors:
		if serveErr == nil {
			result.err = errors.New("ChatGPT callback server stopped before login completed")
		} else {
			result.err = fmt.Errorf("serve ChatGPT OAuth callback: %w", serveErr)
		}
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = server.Shutdown(shutdownContext)
	cancel()
	if result.err != nil {
		return ProfileInfo{}, result.err
	}
	return result.profile, nil
}

func (service *Service) listenCallback() (net.Listener, int, error) {
	var failures []error
	for _, port := range service.callbackPorts {
		listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		return listener, listener.Addr().(*net.TCPAddr).Port, nil
	}
	return nil, 0, fmt.Errorf("listen for ChatGPT OAuth callback on registered ports: %w", errors.Join(failures...))
}

func (service *Service) authorizationURL(redirectURI, challenge, state string) string {
	values := url.Values{
		"response_type":              {"code"},
		"client_id":                  {service.oauth.clientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {oauthScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {defaultOAuthOriginator},
	}
	return strings.TrimRight(service.oauth.issuer, "/") + "/oauth/authorize?" + values.Encode()
}

func generatePKCE() (verifier string, challenge string, err error) {
	verifier, err = randomURLToken(64)
	if err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func randomURLToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
