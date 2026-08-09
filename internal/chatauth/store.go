// Package chatauth manages QED-owned ChatGPT OAuth credentials
package chatauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/qed-runtime/qed/internal/jsonstrict"
)

const (
	storeVersion      = 1
	maximumStoreBytes = 1 << 20
	lockRetryDelay    = 50 * time.Millisecond
	staleLockAge      = 2 * time.Minute
)

type storedProfile struct {
	Type         string    `json:"type"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	Email        string    `json:"email,omitempty"`
	Plan         string    `json:"plan,omitempty"`
	FedRAMP      bool      `json:"fedramp,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type storeDocument struct {
	Version  int                      `json:"version"`
	Profiles map[string]storedProfile `json:"profiles"`
}

type credentialStore struct {
	path string
	now  func() time.Time
}

func defaultStorePath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	if strings.TrimSpace(directory) == "" {
		return "", errors.New("user configuration directory is empty")
	}
	return filepath.Join(directory, "qed", "auth.json"), nil
}

func newCredentialStore(path string) (*credentialStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("ChatGPT credential store path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve ChatGPT credential store path: %w", err)
	}
	return &credentialStore{path: absolute, now: time.Now}, nil
}

func (store *credentialStore) profile(ctx context.Context, profileID string) (storedProfile, error) {
	if err := ctx.Err(); err != nil {
		return storedProfile{}, err
	}
	if err := validateProfileID(profileID); err != nil {
		return storedProfile{}, err
	}
	document, err := store.read()
	if err != nil {
		return storedProfile{}, err
	}
	profile, ok := document.Profiles[profileID]
	if !ok {
		return storedProfile{}, fmt.Errorf("ChatGPT auth profile %q is not logged in", profileID)
	}
	if err := validateStoredProfile(profileID, profile); err != nil {
		return storedProfile{}, err
	}
	return profile, nil
}

func (store *credentialStore) profiles(ctx context.Context) (map[string]storedProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	document, err := store.read()
	if err != nil {
		return nil, err
	}
	profiles := make(map[string]storedProfile, len(document.Profiles))
	for profileID, profile := range document.Profiles {
		if err := validateProfileID(profileID); err != nil {
			return nil, fmt.Errorf("stored ChatGPT auth profile: %w", err)
		}
		if err := validateStoredProfile(profileID, profile); err != nil {
			return nil, err
		}
		profiles[profileID] = profile
	}
	return profiles, nil
}

func (store *credentialStore) update(
	ctx context.Context,
	update func(*storeDocument) (bool, error),
) error {
	release, err := store.acquireLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	document, err := store.read()
	if err != nil {
		return err
	}
	changed, err := update(&document)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return store.write(document)
}

func (store *credentialStore) read() (storeDocument, error) {
	document := storeDocument{Version: storeVersion, Profiles: map[string]storedProfile{}}
	info, err := os.Lstat(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return storeDocument{}, fmt.Errorf("inspect ChatGPT credential store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return storeDocument{}, errors.New("ChatGPT credential store must be a regular file, not a symbolic link")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return storeDocument{}, fmt.Errorf("ChatGPT credential store permissions %04o are too broad, want 0600", info.Mode().Perm())
	}
	file, err := os.Open(store.path)
	if err != nil {
		return storeDocument{}, fmt.Errorf("open ChatGPT credential store: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumStoreBytes+1))
	if err != nil {
		return storeDocument{}, fmt.Errorf("read ChatGPT credential store: %w", err)
	}
	if len(data) > maximumStoreBytes {
		return storeDocument{}, fmt.Errorf("ChatGPT credential store exceeds %d bytes", maximumStoreBytes)
	}
	if err := jsonstrict.Decode(data, maximumStoreBytes, &document); err != nil {
		return storeDocument{}, fmt.Errorf("decode ChatGPT credential store: %w", err)
	}
	if document.Version != storeVersion {
		return storeDocument{}, fmt.Errorf("unsupported ChatGPT credential store version %d", document.Version)
	}
	if document.Profiles == nil {
		document.Profiles = map[string]storedProfile{}
	}
	return document, nil
}

func (store *credentialStore) write(document storeDocument) error {
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create ChatGPT credential directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("secure ChatGPT credential directory: %w", err)
		}
	}
	document.Version = storeVersion
	if document.Profiles == nil {
		document.Profiles = map[string]storedProfile{}
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ChatGPT credential store: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maximumStoreBytes {
		return fmt.Errorf("ChatGPT credential store exceeds %d bytes", maximumStoreBytes)
	}

	temporary, err := os.CreateTemp(directory, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary ChatGPT credential store: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary ChatGPT credential store: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary ChatGPT credential store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary ChatGPT credential store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary ChatGPT credential store: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("replace ChatGPT credential store: %w", err)
	}
	removeTemporary = false
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func (store *credentialStore) acquireLock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return nil, fmt.Errorf("create ChatGPT credential directory: %w", err)
	}
	lockPath := store.path + ".lock"
	for {
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, writeErr := fmt.Fprintf(file, "%d\n", os.Getpid())
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(lockPath)
				return nil, errors.Join(writeErr, closeErr)
			}
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock ChatGPT credential store: %w", err)
		}
		info, statErr := os.Lstat(lockPath)
		if statErr == nil && info.Mode().IsRegular() && store.now().Sub(info.ModTime()) > staleLockAge {
			if removeErr := os.Remove(lockPath); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
				continue
			}
		}
		timer := time.NewTimer(lockRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateProfileID(profileID string) error {
	if profileID == "" {
		return errors.New("ChatGPT auth profile ID is required")
	}
	if !utf8.ValidString(profileID) {
		return errors.New("ChatGPT auth profile ID must be valid UTF-8")
	}
	if strings.TrimSpace(profileID) != profileID {
		return errors.New("ChatGPT auth profile ID must not have leading or trailing whitespace")
	}
	for _, character := range profileID {
		if unicode.IsControl(character) {
			return errors.New("ChatGPT auth profile ID must not contain control characters")
		}
	}
	return nil
}

func validateStoredProfile(profileID string, profile storedProfile) error {
	if profile.Type != "chatgpt" {
		return fmt.Errorf("ChatGPT auth profile %q has unsupported type %q", profileID, profile.Type)
	}
	if profile.AccessToken == "" || profile.RefreshToken == "" || profile.IDToken == "" || profile.AccountID == "" {
		return fmt.Errorf("ChatGPT auth profile %q is incomplete, log in again", profileID)
	}
	if profile.ExpiresAt.IsZero() {
		return fmt.Errorf("ChatGPT auth profile %q has no access token expiration, log in again", profileID)
	}
	return nil
}

func sortedProfileIDs(profiles map[string]storedProfile) []string {
	ids := make([]string, 0, len(profiles))
	for profileID := range profiles {
		ids = append(ids, profileID)
	}
	sort.Strings(ids)
	return ids
}
