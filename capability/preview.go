package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumApprovalPreviewBytes        = 6 << 10
	maximumApprovalPreviewSummaryBytes = 512
	maximumApprovalPreviewDetails      = 64
	maximumApprovalPreviewLabelBytes   = 64
	maximumApprovalPreviewValueBytes   = 4 << 10
)

// ApprovalPreview contains bounded, human-readable facts about one Tool call
// that a host may show before granting capabilities
//
// Preview content is derived from Tool arguments and may contain paths,
// commands, or other sensitive invocation metadata. Callers must protect it
// like the corresponding Tool Call
type ApprovalPreview struct {
	// Summary identifies the operation at a glance
	Summary string `json:"summary"`
	// Details contains bounded labeled facts such as paths, argv, and timeouts
	Details []ApprovalPreviewDetail `json:"details,omitempty"`
}

// ApprovalPreviewDetail is one labeled fact in an ApprovalPreview
type ApprovalPreviewDetail struct {
	// Label identifies the kind of fact in Value
	Label string `json:"label"`
	// Value contains the human-readable fact
	Value string `json:"value"`
}

// ValidateApprovalArgumentsDigest validates the canonical full sha256 digest
// used to bind an approval display to exact Tool arguments
func ValidateApprovalArgumentsDigest(value string) error {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return errors.New("approval argument digest must be a full sha256 digest")
	}
	encoded := value[len(prefix):]
	if encoded != strings.ToLower(encoded) {
		return errors.New("approval argument digest must use lowercase hexadecimal")
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return errors.New("approval argument digest must be a full sha256 digest")
	}
	return nil
}

// ValidateApprovalPreview verifies bounds and rejects terminal control data
func ValidateApprovalPreview(preview *ApprovalPreview) error {
	if preview == nil {
		return nil
	}
	if err := validateApprovalPreviewText("summary", preview.Summary, maximumApprovalPreviewSummaryBytes); err != nil {
		return err
	}
	if strings.TrimSpace(preview.Summary) == "" {
		return errors.New("approval preview summary must not be empty")
	}
	if len(preview.Details) > maximumApprovalPreviewDetails {
		return fmt.Errorf("approval preview has %d details, maximum is %d", len(preview.Details), maximumApprovalPreviewDetails)
	}
	totalBytes := len(preview.Summary)
	for index, detail := range preview.Details {
		if err := validateApprovalPreviewText(
			fmt.Sprintf("detail %d label", index),
			detail.Label,
			maximumApprovalPreviewLabelBytes,
		); err != nil {
			return err
		}
		if strings.TrimSpace(detail.Label) == "" {
			return fmt.Errorf("approval preview detail %d label must not be empty", index)
		}
		if err := validateApprovalPreviewText(
			fmt.Sprintf("detail %d value", index),
			detail.Value,
			maximumApprovalPreviewValueBytes,
		); err != nil {
			return err
		}
		if strings.TrimSpace(detail.Value) == "" {
			return fmt.Errorf("approval preview detail %d value must not be empty", index)
		}
		totalBytes += len(detail.Label) + len(detail.Value)
		if totalBytes > maximumApprovalPreviewBytes {
			return fmt.Errorf("approval preview exceeds %d bytes", maximumApprovalPreviewBytes)
		}
	}
	return nil
}

// CloneApprovalPreview returns an isolated copy of preview
func CloneApprovalPreview(preview *ApprovalPreview) *ApprovalPreview {
	if preview == nil {
		return nil
	}
	result := *preview
	result.Details = append([]ApprovalPreviewDetail(nil), preview.Details...)
	return &result
}

func validateApprovalPreviewText(name, value string, maximumBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("approval preview %s must be valid UTF-8", name)
	}
	if len(value) > maximumBytes {
		return fmt.Errorf("approval preview %s exceeds %d bytes", name, maximumBytes)
	}
	if strings.IndexFunc(value, unsafeApprovalPreviewRune) >= 0 {
		return fmt.Errorf("approval preview %s contains terminal control data", name)
	}
	return nil
}

func unsafeApprovalPreviewRune(value rune) bool {
	return unicode.IsControl(value) || unicode.In(value, unicode.Cf)
}
