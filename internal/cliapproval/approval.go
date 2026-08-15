// Package cliapproval provides the terminal approval Adapter for QED CLI
package cliapproval

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/qed-runtime/qed/capability"
)

const (
	maximumApprovalLineBytes     = 4096
	maximumApprovalCapabilities  = 64
	maximumApprovalIdentityBytes = 512
)

// Approver serializes yes-or-no approval prompts over one CLI input stream
type Approver struct {
	input  io.Reader
	output io.Writer
	mu     sync.Mutex
	once   sync.Once
	lines  chan lineResult
}

type lineResult struct {
	line string
	err  error
}

// New validates streams and constructs a lazy terminal Approver
func New(input io.Reader, output io.Writer) (*Approver, error) {
	if input == nil || output == nil {
		return nil, errors.New("approval input and output are required")
	}
	return &Approver{input: input, output: output}, nil
}

// Approve prompts until the user answers yes or no, EOF rejects safely
func (approver *Approver) Approve(ctx context.Context, request capability.Request) (bool, error) {
	if ctx == nil {
		return false, errors.New("approval context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateApprovalRequest(request); err != nil {
		return false, err
	}
	approver.mu.Lock()
	defer approver.mu.Unlock()
	approver.once.Do(approver.startReader)

	capabilities := make([]string, len(request.Capabilities))
	for index, name := range request.Capabilities {
		capabilities[index] = string(name)
	}
	sort.Strings(capabilities)
	if _, err := fmt.Fprintf(
		approver.output,
		"Approval required for Tool %q with capabilities [%s]\n",
		request.Tool,
		strings.Join(capabilities, ", "),
	); err != nil {
		return false, fmt.Errorf("write approval prompt: %w", err)
	}
	if request.ExtensionID != "" {
		if _, err := fmt.Fprintf(
			approver.output,
			"Extension: %s generation %d\n",
			request.ExtensionID,
			request.ExtensionGeneration,
		); err != nil {
			return false, fmt.Errorf("write approval Extension: %w", err)
		}
	}
	if request.Preview == nil {
		if _, err := io.WriteString(approver.output, "Details: unavailable\n"); err != nil {
			return false, fmt.Errorf("write approval details: %w", err)
		}
	} else {
		if _, err := fmt.Fprintf(approver.output, "Action: %s\n", request.Preview.Summary); err != nil {
			return false, fmt.Errorf("write approval summary: %w", err)
		}
		for _, detail := range request.Preview.Details {
			if _, err := fmt.Fprintf(approver.output, "  %s: %s\n", detail.Label, detail.Value); err != nil {
				return false, fmt.Errorf("write approval detail: %w", err)
			}
		}
	}
	if request.ArgumentsDigest != "" {
		if _, err := fmt.Fprintf(approver.output, "Arguments: %s\n", request.ArgumentsDigest); err != nil {
			return false, fmt.Errorf("write approval argument digest: %w", err)
		}
	}
	if _, err := io.WriteString(approver.output, "Approve? [y/N] "); err != nil {
		return false, fmt.Errorf("write approval prompt: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case result, ok := <-approver.lines:
			if !ok || errors.Is(result.err, io.EOF) {
				return false, nil
			}
			if result.err != nil {
				return false, fmt.Errorf("read approval response: %w", result.err)
			}
			switch strings.ToLower(strings.TrimSpace(result.line)) {
			case "y", "yes":
				return true, nil
			case "", "n", "no":
				return false, nil
			default:
				if _, err := io.WriteString(approver.output, "Please answer yes or no\nApprove? [y/N] "); err != nil {
					return false, fmt.Errorf("write approval prompt: %w", err)
				}
			}
		}
	}
}

func validateApprovalRequest(request capability.Request) error {
	if err := validateApprovalIdentity("Tool", request.Tool); err != nil {
		return err
	}
	if len(request.Capabilities) > maximumApprovalCapabilities {
		return fmt.Errorf(
			"approval has %d capabilities, maximum is %d",
			len(request.Capabilities),
			maximumApprovalCapabilities,
		)
	}
	for _, name := range request.Capabilities {
		if err := capability.ValidateName(name); err != nil {
			return fmt.Errorf("approval capability: %w", err)
		}
	}
	if request.ArgumentsDigest != "" {
		if err := capability.ValidateApprovalArgumentsDigest(request.ArgumentsDigest); err != nil {
			return err
		}
	}
	if request.ExtensionGeneration != 0 && request.ExtensionID == "" {
		return errors.New("approval Extension generation requires an Extension ID")
	}
	if request.ExtensionID != "" {
		if err := validateApprovalIdentity("Extension ID", request.ExtensionID); err != nil {
			return err
		}
	}
	if err := capability.ValidateApprovalPreview(request.Preview); err != nil {
		return fmt.Errorf("validate approval preview: %w", err)
	}
	return nil
}

func validateApprovalIdentity(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("approval %s is required and must not have surrounding whitespace", name)
	}
	if !utf8.ValidString(value) || len(value) > maximumApprovalIdentityBytes || strings.IndexFunc(value, unsafeApprovalIdentityRune) >= 0 {
		return fmt.Errorf("approval %s must be bounded valid UTF-8 without control data", name)
	}
	return nil
}

func unsafeApprovalIdentityRune(value rune) bool {
	return unicode.IsControl(value) || unicode.In(value, unicode.Cf)
}

func (approver *Approver) startReader() {
	approver.lines = make(chan lineResult, 1)
	go func() {
		defer close(approver.lines)
		scanner := bufio.NewScanner(approver.input)
		scanner.Buffer(make([]byte, 256), maximumApprovalLineBytes)
		for scanner.Scan() {
			approver.lines <- lineResult{line: scanner.Text()}
		}
		if err := scanner.Err(); err != nil {
			approver.lines <- lineResult{err: err}
			return
		}
		approver.lines <- lineResult{err: io.EOF}
	}()
}
