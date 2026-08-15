package capability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/qed-runtime/qed/agent"
)

// WaitApprover suspends the current Run and resolves approval through Run Events
//
// The persisted wait payload contains the Tool identity, capability names,
// exact argument digest, Extension generation, and an optional bounded preview.
// Raw Tool arguments are intentionally excluded
type WaitApprover struct{}

// Approve waits for a matching RunHandle.Resume response
func (WaitApprover) Approve(ctx context.Context, request Request) (bool, error) {
	if err := ValidateApprovalPreview(request.Preview); err != nil {
		return false, fmt.Errorf("validate approval preview: %w", err)
	}
	capabilities := make([]string, len(request.Capabilities))
	for index, name := range request.Capabilities {
		capabilities[index] = string(name)
	}
	sort.Strings(capabilities)
	argumentsDigest, err := approvalArgumentsDigest(request)
	if err != nil {
		return false, err
	}
	request.ArgumentsDigest = argumentsDigest
	payload, err := json.Marshal(struct {
		Tool                string           `json:"tool"`
		Capabilities        []string         `json:"capabilities"`
		ArgumentsDigest     string           `json:"arguments_digest"`
		ExtensionID         string           `json:"extension_id,omitempty"`
		ExtensionGeneration uint64           `json:"extension_generation,omitempty"`
		Preview             *ApprovalPreview `json:"preview,omitempty"`
	}{
		Tool:                request.Tool,
		Capabilities:        capabilities,
		ArgumentsDigest:     argumentsDigest,
		ExtensionID:         request.ExtensionID,
		ExtensionGeneration: request.ExtensionGeneration,
		Preview:             CloneApprovalPreview(request.Preview),
	})
	if err != nil {
		return false, fmt.Errorf("encode approval wait request: %w", err)
	}

	response, err := agent.WaitForInput(ctx, agent.WaitRequest{
		ID:      approvalWaitID(request),
		Kind:    agent.WaitKindApproval,
		Prompt:  "Approve the requested Tool capabilities",
		Payload: payload,
	})
	if err != nil {
		return false, err
	}
	var decision struct {
		Approved bool `json:"approved"`
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return false, fmt.Errorf("decode approval wait response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("decode approval wait response: multiple JSON values")
		}
		return false, fmt.Errorf("decode approval wait response trailer: %w", err)
	}
	return decision.Approved, nil
}

func approvalWaitID(request Request) string {
	capabilities := make([]string, len(request.Capabilities))
	for index, name := range request.Capabilities {
		capabilities[index] = string(name)
	}
	sort.Strings(capabilities)
	identity, _ := json.Marshal(struct {
		CallID              string           `json:"call_id"`
		Tool                string           `json:"tool"`
		Capabilities        []string         `json:"capabilities"`
		ArgumentsDigest     string           `json:"arguments_digest"`
		ExtensionID         string           `json:"extension_id,omitempty"`
		ExtensionGeneration uint64           `json:"extension_generation,omitempty"`
		Preview             *ApprovalPreview `json:"preview,omitempty"`
	}{
		CallID:              request.CallID,
		Tool:                request.Tool,
		Capabilities:        capabilities,
		ArgumentsDigest:     request.ArgumentsDigest,
		ExtensionID:         request.ExtensionID,
		ExtensionGeneration: request.ExtensionGeneration,
		Preview:             CloneApprovalPreview(request.Preview),
	})
	digest := sha256.Sum256(identity)
	return "approval_" + hex.EncodeToString(digest[:16])
}

func approvalArgumentsDigest(request Request) (string, error) {
	if request.ArgumentsDigest == "" {
		digest := sha256.Sum256(request.Arguments)
		return "sha256:" + hex.EncodeToString(digest[:]), nil
	}
	if err := ValidateApprovalArgumentsDigest(request.ArgumentsDigest); err != nil {
		return "", err
	}
	return request.ArgumentsDigest, nil
}
