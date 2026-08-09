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
// The persisted wait payload contains only the Tool name and capability names.
// Raw Tool arguments are intentionally excluded.
type WaitApprover struct{}

// Approve waits for a matching RunHandle.Resume response
func (WaitApprover) Approve(ctx context.Context, request Request) (bool, error) {
	capabilities := make([]string, len(request.Capabilities))
	for index, name := range request.Capabilities {
		capabilities[index] = string(name)
	}
	sort.Strings(capabilities)
	payload, err := json.Marshal(struct {
		Tool         string   `json:"tool"`
		Capabilities []string `json:"capabilities"`
	}{Tool: request.Tool, Capabilities: capabilities})
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
	value := request.CallID + "\x00" + request.Tool
	digest := sha256.Sum256([]byte(value))
	return "approval_" + hex.EncodeToString(digest[:16])
}
