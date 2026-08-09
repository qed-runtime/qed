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

	"github.com/qed-runtime/qed/capability"
)

const maximumApprovalLineBytes = 4096

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
		"Approval required for Tool %q with capabilities [%s]\nApprove? [y/N] ",
		request.Tool,
		strings.Join(capabilities, ", "),
	); err != nil {
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
