package chatauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/qed-runtime/qed/provider/openaicodex"
)

const refreshWindow = 5 * time.Minute

// ProfileInfo describes a stored ChatGPT auth profile without exposing secrets
type ProfileInfo struct {
	ID        string
	Email     string
	Plan      string
	ExpiresAt time.Time
	UpdatedAt time.Time
}

// LogoutResult reports local removal and best-effort remote revocation
type LogoutResult struct {
	Removed         bool
	RevocationError error
}

// Service manages the QED ChatGPT credential store and OAuth lifecycle
type Service struct {
	store         *credentialStore
	oauth         oauthClient
	now           func() time.Time
	callbackPorts []int
	wait          func(context.Context, time.Duration) error
}

// New creates a ChatGPT auth Service backed by the supplied credential file
func New(storePath string) (*Service, error) {
	store, err := newCredentialStore(storePath)
	if err != nil {
		return nil, err
	}
	return newService(store, oauthClient{
		issuer:   defaultIssuer,
		clientID: defaultClientID,
		client:   &http.Client{Timeout: 30 * time.Second},
	}), nil
}

// NewDefault creates a ChatGPT auth Service in the OS user configuration directory
func NewDefault() (*Service, error) {
	path, err := defaultStorePath()
	if err != nil {
		return nil, err
	}
	return New(path)
}

func newService(store *credentialStore, oauth oauthClient) *Service {
	return &Service{
		store:         store,
		oauth:         oauth,
		now:           time.Now,
		callbackPorts: []int{1455, 1457},
		wait: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// CredentialSource returns a concurrent authorization source for one named profile
func (service *Service) CredentialSource(profileID string) (*CredentialSource, error) {
	if err := validateProfileID(profileID); err != nil {
		return nil, err
	}
	return &CredentialSource{service: service, profileID: profileID}, nil
}

// Profiles lists stored profiles without returning any credential values
func (service *Service) Profiles(ctx context.Context) ([]ProfileInfo, error) {
	profiles, err := service.store.profiles(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ProfileInfo, 0, len(profiles))
	for _, profileID := range sortedProfileIDs(profiles) {
		profile := profiles[profileID]
		result = append(result, ProfileInfo{
			ID: profileID, Email: profile.Email, Plan: profile.Plan,
			ExpiresAt: profile.ExpiresAt, UpdatedAt: profile.UpdatedAt,
		})
	}
	return result, nil
}

// ValidateProfile confirms that a named profile exists and is structurally valid
func (service *Service) ValidateProfile(ctx context.Context, profileID string) error {
	_, err := service.store.profile(ctx, profileID)
	return err
}

// Logout removes one local profile and then attempts remote token revocation
func (service *Service) Logout(ctx context.Context, profileID string) (LogoutResult, error) {
	if err := validateProfileID(profileID); err != nil {
		return LogoutResult{}, err
	}
	var removed storedProfile
	found := false
	err := service.store.update(ctx, func(document *storeDocument) (bool, error) {
		removed, found = document.Profiles[profileID]
		if !found {
			return false, nil
		}
		delete(document.Profiles, profileID)
		return true, nil
	})
	if err != nil {
		return LogoutResult{}, err
	}
	result := LogoutResult{Removed: found}
	if !found {
		return result, nil
	}
	revokeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	result.RevocationError = service.oauth.revoke(revokeContext, removed)
	cancel()
	return result, nil
}

// CredentialSource resolves and refreshes one named ChatGPT auth profile
type CredentialSource struct {
	service   *Service
	profileID string
}

// Authorization resolves a current ChatGPT Codex authorization
func (source *CredentialSource) Authorization(ctx context.Context) (openaicodex.Authorization, error) {
	return source.resolve(ctx, "", false)
}

// RecoverUnauthorized refreshes or reloads authorization after one HTTP 401
func (source *CredentialSource) RecoverUnauthorized(
	ctx context.Context,
	rejected openaicodex.Authorization,
) (openaicodex.Authorization, error) {
	return source.resolve(ctx, rejected.AccessToken, true)
}

func (source *CredentialSource) resolve(
	ctx context.Context,
	rejectedToken string,
	force bool,
) (openaicodex.Authorization, error) {
	var authorization openaicodex.Authorization
	err := source.service.store.update(ctx, func(document *storeDocument) (bool, error) {
		profile, ok := document.Profiles[source.profileID]
		if !ok {
			return false, fmt.Errorf("ChatGPT auth profile %q is not logged in", source.profileID)
		}
		if err := validateStoredProfile(source.profileID, profile); err != nil {
			return false, err
		}
		if force && rejectedToken != "" && profile.AccessToken != rejectedToken {
			authorization = authorizationFromProfile(profile)
			return false, nil
		}
		now := source.service.now()
		if !force && profile.ExpiresAt.After(now.Add(refreshWindow)) {
			authorization = authorizationFromProfile(profile)
			return false, nil
		}

		refreshed, err := source.service.oauth.refresh(ctx, profile.RefreshToken)
		if err != nil {
			if !force && profile.ExpiresAt.After(now) {
				authorization = authorizationFromProfile(profile)
				return false, nil
			}
			return false, err
		}
		updated := profile
		if refreshed.IDToken != "" {
			updated.IDToken = refreshed.IDToken
		}
		if refreshed.AccessToken != "" {
			updated.AccessToken = refreshed.AccessToken
		}
		if refreshed.RefreshToken != "" {
			updated.RefreshToken = refreshed.RefreshToken
		}
		identity, expiresAt, err := identityFromTokens(updated.IDToken, updated.AccessToken)
		if err != nil {
			return false, fmt.Errorf("validate refreshed ChatGPT credentials: %w", err)
		}
		if identity.AccountID != profile.AccountID {
			return false, errors.New("refreshed ChatGPT credentials changed account, log in again")
		}
		updated.AccountID = identity.AccountID
		updated.Email = identity.Email
		updated.Plan = identity.Plan
		updated.FedRAMP = identity.FedRAMP
		updated.ExpiresAt = expiresAt
		updated.UpdatedAt = now.UTC()
		document.Profiles[source.profileID] = updated
		authorization = authorizationFromProfile(updated)
		return true, nil
	})
	if err != nil {
		return openaicodex.Authorization{}, err
	}
	return authorization, nil
}

func authorizationFromProfile(profile storedProfile) openaicodex.Authorization {
	return openaicodex.Authorization{
		AccessToken: profile.AccessToken,
		AccountID:   profile.AccountID,
		FedRAMP:     profile.FedRAMP,
	}
}

func (service *Service) saveTokens(
	ctx context.Context,
	profileID string,
	tokens exchangedTokens,
) (ProfileInfo, error) {
	if err := validateProfileID(profileID); err != nil {
		return ProfileInfo{}, err
	}
	identity, expiresAt, err := identityFromTokens(tokens.IDToken, tokens.AccessToken)
	if err != nil {
		return ProfileInfo{}, fmt.Errorf("validate ChatGPT credentials: %w", err)
	}
	now := service.now().UTC()
	profile := storedProfile{
		Type: "chatgpt", IDToken: tokens.IDToken, AccessToken: tokens.AccessToken,
		RefreshToken: tokens.RefreshToken, AccountID: identity.AccountID,
		Email: identity.Email, Plan: identity.Plan, FedRAMP: identity.FedRAMP,
		ExpiresAt: expiresAt, UpdatedAt: now,
	}
	if err := service.store.update(ctx, func(document *storeDocument) (bool, error) {
		document.Profiles[profileID] = profile
		return true, nil
	}); err != nil {
		return ProfileInfo{}, err
	}
	return ProfileInfo{
		ID: profileID, Email: profile.Email, Plan: profile.Plan,
		ExpiresAt: profile.ExpiresAt, UpdatedAt: profile.UpdatedAt,
	}, nil
}
