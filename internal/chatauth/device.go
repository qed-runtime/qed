package chatauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const deviceLoginTimeout = 15 * time.Minute

// DeviceCode contains the user-facing data for a headless login
type DeviceCode struct {
	VerificationURL string
	UserCode        string
}

// DeviceLoginOptions configures a headless device-code login
type DeviceLoginOptions struct {
	// PresentCode receives the verification URL and one-time code before polling
	PresentCode func(DeviceCode)
}

type deviceAuthorization struct {
	VerificationURL string
	UserCode        string
	DeviceAuthID    string
	Interval        time.Duration
}

type intervalSeconds time.Duration

func (interval *intervalSeconds) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		seconds, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return err
		}
		*interval = intervalSeconds(time.Duration(seconds) * time.Second)
		return nil
	}
	var seconds uint64
	if err := json.Unmarshal(data, &seconds); err != nil {
		return err
	}
	*interval = intervalSeconds(time.Duration(seconds) * time.Second)
	return nil
}

// LoginDevice signs in one named profile with the headless device-code flow
func (service *Service) LoginDevice(
	ctx context.Context,
	profileID string,
	options DeviceLoginOptions,
) (ProfileInfo, error) {
	if ctx == nil {
		return ProfileInfo{}, errors.New("ChatGPT device login context must not be nil")
	}
	if err := validateProfileID(profileID); err != nil {
		return ProfileInfo{}, err
	}
	loginContext, cancel := context.WithTimeout(ctx, deviceLoginTimeout)
	defer cancel()
	authorization, err := service.requestDeviceCode(loginContext)
	if err != nil {
		return ProfileInfo{}, err
	}
	if options.PresentCode != nil {
		options.PresentCode(DeviceCode{
			VerificationURL: authorization.VerificationURL,
			UserCode:        authorization.UserCode,
		})
	}
	code, verifier, err := service.pollDeviceCode(loginContext, authorization)
	if err != nil {
		return ProfileInfo{}, err
	}
	redirectURI := strings.TrimRight(service.oauth.issuer, "/") + "/deviceauth/callback"
	tokens, err := service.oauth.exchange(loginContext, code, redirectURI, verifier)
	if err != nil {
		return ProfileInfo{}, fmt.Errorf("complete ChatGPT device login: %w", err)
	}
	return service.saveTokens(loginContext, profileID, tokens)
}

func (service *Service) requestDeviceCode(ctx context.Context) (deviceAuthorization, error) {
	payload, err := json.Marshal(struct {
		ClientID string `json:"client_id"`
	}{ClientID: service.oauth.clientID})
	if err != nil {
		return deviceAuthorization{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(service.oauth.issuer, "/")+"/api/accounts/deviceauth/usercode",
		bytes.NewReader(payload),
	)
	if err != nil {
		return deviceAuthorization{}, fmt.Errorf("create ChatGPT device code request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "qed-runtime")
	var response struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeOld  string          `json:"usercode"`
		Interval     intervalSeconds `json:"interval"`
	}
	if err := service.oauth.doJSON(request, &response); err != nil {
		return deviceAuthorization{}, fmt.Errorf("request ChatGPT device code: %w", err)
	}
	if response.UserCode == "" {
		response.UserCode = response.UserCodeOld
	}
	if response.DeviceAuthID == "" || response.UserCode == "" {
		return deviceAuthorization{}, errors.New("ChatGPT device code response is incomplete")
	}
	interval := time.Duration(response.Interval)
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return deviceAuthorization{
		VerificationURL: strings.TrimRight(service.oauth.issuer, "/") + "/codex/device",
		UserCode:        response.UserCode,
		DeviceAuthID:    response.DeviceAuthID,
		Interval:        interval,
	}, nil
}

func (service *Service) pollDeviceCode(
	ctx context.Context,
	authorization deviceAuthorization,
) (code string, verifier string, err error) {
	for {
		payload, marshalErr := json.Marshal(struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		}{DeviceAuthID: authorization.DeviceAuthID, UserCode: authorization.UserCode})
		if marshalErr != nil {
			return "", "", marshalErr
		}
		request, requestErr := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			strings.TrimRight(service.oauth.issuer, "/")+"/api/accounts/deviceauth/token",
			bytes.NewReader(payload),
		)
		if requestErr != nil {
			return "", "", fmt.Errorf("create ChatGPT device token request: %w", requestErr)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "qed-runtime")
		response, requestErr := service.oauth.httpClient().Do(request)
		if requestErr != nil {
			return "", "", fmt.Errorf("poll ChatGPT device login: %w", requestErr)
		}
		if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumOAuthBodyBytes))
			_ = response.Body.Close()
			if waitErr := service.wait(ctx, authorization.Interval); waitErr != nil {
				return "", "", waitErr
			}
			continue
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			requestErr = decodeOAuthError(response)
			_ = response.Body.Close()
			return "", "", fmt.Errorf("poll ChatGPT device login: %w", requestErr)
		}
		var result struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeChallenge     string `json:"code_challenge"`
			CodeVerifier      string `json:"code_verifier"`
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, maximumOAuthBodyBytes+1))
		decodeErr := decoder.Decode(&result)
		_ = response.Body.Close()
		if decodeErr != nil {
			return "", "", fmt.Errorf("decode ChatGPT device token response: %w", decodeErr)
		}
		if result.AuthorizationCode == "" || result.CodeChallenge == "" || result.CodeVerifier == "" {
			return "", "", errors.New("ChatGPT device token response is incomplete")
		}
		digest := sha256.Sum256([]byte(result.CodeVerifier))
		calculated := base64.RawURLEncoding.EncodeToString(digest[:])
		if subtle.ConstantTimeCompare([]byte(calculated), []byte(result.CodeChallenge)) != 1 {
			return "", "", errors.New("ChatGPT device token response has an invalid PKCE challenge")
		}
		return result.AuthorizationCode, result.CodeVerifier, nil
	}
}
