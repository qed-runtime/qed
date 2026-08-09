package chatauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type tokenClaims struct {
	Email   string `json:"email"`
	Expires int64  `json:"exp"`
	Profile *struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
	Auth *struct {
		AccountID string `json:"chatgpt_account_id"`
		Plan      string `json:"chatgpt_plan_type"`
		UserID    string `json:"chatgpt_user_id"`
		LegacyID  string `json:"user_id"`
		FedRAMP   bool   `json:"chatgpt_account_is_fedramp"`
	} `json:"https://api.openai.com/auth"`
}

type tokenIdentity struct {
	AccountID string
	Email     string
	Plan      string
	FedRAMP   bool
}

func decodeTokenClaims(token string) (tokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return tokenClaims{}, errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenClaims{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	if len(payload) > maximumStoreBytes {
		return tokenClaims{}, errors.New("JWT payload exceeds size limit")
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return tokenClaims{}, fmt.Errorf("decode JWT claims: %w", err)
	}
	return claims, nil
}

func identityFromTokens(idToken, accessToken string) (tokenIdentity, time.Time, error) {
	idClaims, err := decodeTokenClaims(idToken)
	if err != nil {
		return tokenIdentity{}, time.Time{}, fmt.Errorf("decode ChatGPT ID token: %w", err)
	}
	accessClaims, err := decodeTokenClaims(accessToken)
	if err != nil {
		return tokenIdentity{}, time.Time{}, fmt.Errorf("decode ChatGPT access token: %w", err)
	}
	expiresAt := time.Unix(accessClaims.Expires, 0).UTC()
	if accessClaims.Expires <= 0 || expiresAt.IsZero() {
		return tokenIdentity{}, time.Time{}, errors.New("ChatGPT access token has no valid expiration")
	}
	identity := identityFromClaims(idClaims)
	accessIdentity := identityFromClaims(accessClaims)
	if identity.AccountID == "" {
		identity.AccountID = accessIdentity.AccountID
	}
	if identity.Email == "" {
		identity.Email = accessIdentity.Email
	}
	if identity.Plan == "" {
		identity.Plan = accessIdentity.Plan
	}
	identity.FedRAMP = identity.FedRAMP || accessIdentity.FedRAMP
	if identity.AccountID == "" {
		return tokenIdentity{}, time.Time{}, errors.New("ChatGPT tokens do not contain an account ID")
	}
	return identity, expiresAt, nil
}

func identityFromClaims(claims tokenClaims) tokenIdentity {
	identity := tokenIdentity{Email: claims.Email}
	if identity.Email == "" && claims.Profile != nil {
		identity.Email = claims.Profile.Email
	}
	if claims.Auth != nil {
		identity.AccountID = claims.Auth.AccountID
		identity.Plan = claims.Auth.Plan
		identity.FedRAMP = claims.Auth.FedRAMP
	}
	return identity
}
