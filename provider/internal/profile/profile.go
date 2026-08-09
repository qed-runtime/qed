// Package profile validates and formats Provider profile identifiers
package profile

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Name adds an optional profile ID to a Provider protocol name
func Name(protocolName, profileID string) (string, error) {
	if profileID == "" {
		return protocolName, nil
	}
	if !utf8.ValidString(profileID) {
		return "", errors.New("profile ID must be valid UTF-8")
	}
	if strings.TrimSpace(profileID) != profileID {
		return "", errors.New("profile ID must not have leading or trailing whitespace")
	}
	for _, character := range profileID {
		if unicode.IsControl(character) {
			return "", errors.New("profile ID must not contain control characters")
		}
	}
	return protocolName + ":" + profileID, nil
}
