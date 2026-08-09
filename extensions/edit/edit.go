// Package edit provides a preconditioned patch Tool scoped to one Workspace
package edit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extensions/internal/filetext"
	"github.com/qed-runtime/qed/internal/jsonstrict"
	"github.com/qed-runtime/qed/workspace"
)

const (
	// ApplyPatchToolName is the model-facing name of the preconditioned patch Tool
	ApplyPatchToolName = "apply_patch"

	defaultMaxPatchBytes = 1 << 20
	defaultMaxFileBytes  = 4 << 20
	defaultMaxFiles      = 64
	maximumRequestBytes  = 2 << 20
)

var (
	hunkHeaderPattern = regexp.MustCompile(`\A@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)
	digestPattern     = regexp.MustCompile(`\Asha256:[0-9a-f]{64}\z`)
)

// Options bounds apply_patch resource use
type Options struct {
	MaxPatchBytes int
	MaxFileBytes  int64
	MaxFiles      int
}

// NewTool constructs apply_patch for one Workspace
func NewTool(scoped *workspace.Workspace, options Options) (agent.Tool, error) {
	if scoped == nil {
		return nil, errors.New("edit Workspace is required")
	}
	maxPatchBytes := options.MaxPatchBytes
	if maxPatchBytes == 0 {
		maxPatchBytes = defaultMaxPatchBytes
	}
	maxFileBytes := options.MaxFileBytes
	if maxFileBytes == 0 {
		maxFileBytes = defaultMaxFileBytes
	}
	maxFiles := options.MaxFiles
	if maxFiles == 0 {
		maxFiles = defaultMaxFiles
	}
	if maxPatchBytes <= 0 || maxFileBytes <= 0 || maxFiles <= 0 {
		return nil, errors.New("edit Tool limits must be positive")
	}
	return &applyPatchTool{
		workspace:     scoped,
		maxPatchBytes: maxPatchBytes,
		maxFileBytes:  maxFileBytes,
		maxFiles:      maxFiles,
	}, nil
}

type applyPatchTool struct {
	workspace     *workspace.Workspace
	maxPatchBytes int
	maxFileBytes  int64
	maxFiles      int
}

func (tool *applyPatchTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         ApplyPatchToolName,
		Description:  "Apply a bounded unified diff after verifying explicit file digests or absence",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"patch":{"type":"string"},"preconditions":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"sha256":{"type":"string"},"absent":{"type":"boolean"}},"required":["path"],"additionalProperties":false}}},"required":["patch","preconditions"],"additionalProperties":false}`),
		Capabilities: []string{string(capability.FilesystemWrite)},
	}
}

// RequiredCapabilities requests filesystem.delete when a patch deletes a file
func (tool *applyPatchTool) RequiredCapabilities(_ context.Context, call agent.ToolCall) ([]capability.Name, error) {
	request, patches, err := tool.decode(call.Arguments)
	if err != nil {
		return nil, err
	}
	_ = request
	for _, patch := range patches {
		if patch.kind == changeDelete {
			return []capability.Name{capability.FilesystemDelete}, nil
		}
	}
	return nil, nil
}

func (tool *applyPatchTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	request, patches, err := tool.decode(call.Arguments)
	if err != nil {
		return agent.ToolResult{}, err
	}
	release := tool.workspace.AcquireWrite()
	defer release()
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}

	operations, err := tool.prepareOperations(request.Preconditions, patches)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if err := tool.commit(ctx, operations); err != nil {
		return agent.ToolResult{}, err
	}
	response := patchResponse{Changes: make([]patchChange, len(operations))}
	for index, operation := range operations {
		response.Changes[index] = patchChange{
			Path:         operation.path,
			Kind:         string(operation.kind),
			BeforeDigest: operation.beforeDigest,
			AfterDigest:  digest(operation.after),
			Bytes:        len(operation.after),
		}
		if operation.kind == changeDelete {
			response.Changes[index].AfterDigest = ""
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode apply_patch result: %w", err)
	}
	return agent.ToolResult{Output: string(encoded)}, nil
}

type patchRequest struct {
	Patch         string         `json:"patch"`
	Preconditions []precondition `json:"preconditions"`
}

type precondition struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Absent bool   `json:"absent,omitempty"`
}

func (tool *applyPatchTool) decode(arguments json.RawMessage) (patchRequest, []filePatch, error) {
	var request patchRequest
	if err := jsonstrict.Decode(arguments, maximumRequestBytes, &request); err != nil {
		return patchRequest{}, nil, fmt.Errorf("decode apply_patch arguments: %w", err)
	}
	if request.Patch == "" {
		return patchRequest{}, nil, errors.New("apply_patch patch is required")
	}
	if len(request.Patch) > tool.maxPatchBytes {
		return patchRequest{}, nil, fmt.Errorf("apply_patch patch exceeds %d bytes", tool.maxPatchBytes)
	}
	patches, err := parseUnifiedDiff(request.Patch, tool.maxFiles)
	if err != nil {
		return patchRequest{}, nil, fmt.Errorf("parse apply_patch patch: %w", err)
	}
	return request, patches, nil
}

type changeKind string

const (
	changeAdd    changeKind = "add"
	changeUpdate changeKind = "update"
	changeDelete changeKind = "delete"
)

type filePatch struct {
	path  string
	kind  changeKind
	hunks []hunk
}

type hunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []patchLine
}

type patchLine struct {
	kind      byte
	text      string
	noNewline bool
}

func parseUnifiedDiff(value string, maxFiles int) ([]filePatch, error) {
	lines := strings.Split(value, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var patches []filePatch
	seen := make(map[string]struct{})
	for index := 0; index < len(lines); {
		line := lines[index]
		if line == "" || strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "deleted file mode ") {
			index++
			continue
		}
		if !strings.HasPrefix(line, "--- ") {
			return nil, fmt.Errorf("expected old file header at line %d", index+1)
		}
		oldPath, err := parseHeaderPath(strings.TrimPrefix(line, "--- "))
		if err != nil {
			return nil, fmt.Errorf("old file header line %d: %w", index+1, err)
		}
		index++
		if index >= len(lines) || !strings.HasPrefix(lines[index], "+++ ") {
			return nil, fmt.Errorf("expected new file header at line %d", index+1)
		}
		newPath, err := parseHeaderPath(strings.TrimPrefix(lines[index], "+++ "))
		if err != nil {
			return nil, fmt.Errorf("new file header line %d: %w", index+1, err)
		}
		index++
		path, kind, err := classifyPaths(oldPath, newPath)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("file %q appears more than once", path)
		}
		seen[path] = struct{}{}
		patch := filePatch{path: path, kind: kind}
		for index < len(lines) && strings.HasPrefix(lines[index], "@@ ") {
			parsed, next, err := parseHunk(lines, index)
			if err != nil {
				return nil, err
			}
			patch.hunks = append(patch.hunks, parsed)
			index = next
		}
		if len(patch.hunks) == 0 && kind == changeUpdate {
			return nil, fmt.Errorf("updated file %q has no hunks", path)
		}
		patches = append(patches, patch)
		if len(patches) > maxFiles {
			return nil, fmt.Errorf("patch exceeds %d files", maxFiles)
		}
	}
	if len(patches) == 0 {
		return nil, errors.New("patch contains no file changes")
	}
	return patches, nil
}

func parseHeaderPath(value string) (string, error) {
	if before, _, found := strings.Cut(value, "\t"); found {
		value = before
	}
	if value == "/dev/null" {
		return value, nil
	}
	if strings.HasPrefix(value, "\"") {
		return "", errors.New("quoted patch paths are not supported")
	}
	if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
		value = value[2:]
	}
	return normalizePatchPath(value)
}

func normalizePatchPath(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("patch path must be non-empty valid UTF-8 without NUL")
	}
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", errors.New("patch path must be relative")
	}
	cleaned := filepath.Clean(filepath.FromSlash(value))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("patch path must identify a file within the workspace")
	}
	return filepath.ToSlash(cleaned), nil
}

func classifyPaths(oldPath, newPath string) (string, changeKind, error) {
	switch {
	case oldPath == "/dev/null" && newPath == "/dev/null":
		return "", "", errors.New("both patch paths must not be /dev/null")
	case oldPath == "/dev/null":
		return newPath, changeAdd, nil
	case newPath == "/dev/null":
		return oldPath, changeDelete, nil
	case oldPath != newPath:
		return "", "", fmt.Errorf("patch rename from %q to %q is not supported", oldPath, newPath)
	default:
		return oldPath, changeUpdate, nil
	}
}

func parseHunk(lines []string, index int) (hunk, int, error) {
	match := hunkHeaderPattern.FindStringSubmatch(lines[index])
	if match == nil {
		return hunk{}, index, fmt.Errorf("invalid hunk header at line %d", index+1)
	}
	oldStart, _ := strconv.Atoi(match[1])
	oldCount := parseCount(match[2])
	newStart, _ := strconv.Atoi(match[3])
	newCount := parseCount(match[4])
	parsed := hunk{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
	index++
	oldSeen := 0
	newSeen := 0
	for oldSeen < oldCount || newSeen < newCount {
		if index >= len(lines) || lines[index] == "" {
			return hunk{}, index, fmt.Errorf("hunk at line %d ended before its declared counts", index+1)
		}
		line := patchLine{kind: lines[index][0], text: lines[index][1:]}
		switch line.kind {
		case ' ':
			oldSeen++
			newSeen++
		case '-':
			oldSeen++
		case '+':
			newSeen++
		default:
			return hunk{}, index, fmt.Errorf("invalid hunk line prefix at line %d", index+1)
		}
		if oldSeen > oldCount || newSeen > newCount {
			return hunk{}, index, fmt.Errorf("hunk at line %d exceeds its declared counts", index+1)
		}
		parsed.lines = append(parsed.lines, line)
		index++
		if index < len(lines) && lines[index] == `\ No newline at end of file` {
			parsed.lines[len(parsed.lines)-1].noNewline = true
			index++
		}
	}
	return parsed, index, nil
}

func parseCount(value string) int {
	if value == "" {
		return 1
	}
	parsed, _ := strconv.Atoi(value)
	return parsed
}

type operation struct {
	path         string
	kind         changeKind
	before       []byte
	after        []byte
	beforeDigest string
	mode         os.FileMode
}

func (tool *applyPatchTool) prepareOperations(preconditions []precondition, patches []filePatch) ([]operation, error) {
	conditions := make(map[string]precondition, len(preconditions))
	for _, condition := range preconditions {
		path, err := normalizePatchPath(condition.Path)
		if err != nil {
			return nil, fmt.Errorf("precondition path %q: %w", condition.Path, err)
		}
		if condition.Absent == (condition.SHA256 != "") {
			return nil, fmt.Errorf("precondition %q must specify exactly one of sha256 or absent", path)
		}
		if condition.SHA256 != "" && !digestPattern.MatchString(condition.SHA256) {
			return nil, fmt.Errorf("precondition %q has an invalid sha256 digest", path)
		}
		if _, duplicate := conditions[path]; duplicate {
			return nil, fmt.Errorf("precondition %q appears more than once", path)
		}
		condition.Path = path
		conditions[path] = condition
	}
	if len(conditions) != len(patches) {
		return nil, errors.New("apply_patch requires exactly one precondition for every changed file")
	}

	operations := make([]operation, 0, len(patches))
	for _, patch := range patches {
		condition, ok := conditions[patch.path]
		if !ok {
			return nil, fmt.Errorf("apply_patch precondition for %q is required", patch.path)
		}
		_, exists, err := tool.workspace.ResolveTarget(patch.path)
		if err != nil {
			return nil, err
		}
		operation := operation{path: patch.path, kind: patch.kind, mode: 0o644}
		switch patch.kind {
		case changeAdd:
			if exists || !condition.Absent {
				return nil, fmt.Errorf("added file %q must be absent", patch.path)
			}
		case changeUpdate, changeDelete:
			if !exists || condition.Absent {
				return nil, fmt.Errorf("existing file %q requires a sha256 precondition", patch.path)
			}
			document, err := readDocument(tool.workspace, patch.path, tool.maxFileBytes)
			if err != nil {
				return nil, fmt.Errorf("read patch preimage %q: %w", patch.path, err)
			}
			if document.Digest != condition.SHA256 {
				return nil, fmt.Errorf("patch precondition for %q does not match the current file", patch.path)
			}
			operation.before = append([]byte(nil), document.Data...)
			operation.beforeDigest = document.Digest
			operation.mode = document.Mode
		}
		operation.after, err = applyHunks(operation.before, patch)
		if err != nil {
			return nil, fmt.Errorf("apply patch to %q: %w", patch.path, err)
		}
		if int64(len(operation.after)) > tool.maxFileBytes {
			return nil, fmt.Errorf("patched file %q exceeds %d bytes", patch.path, tool.maxFileBytes)
		}
		if patch.kind == changeDelete && len(operation.after) != 0 {
			return nil, fmt.Errorf("deleted file %q patch does not remove all content", patch.path)
		}
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(first, second int) bool { return operations[first].path < operations[second].path })
	return operations, nil
}

func applyHunks(before []byte, patch filePatch) ([]byte, error) {
	source, sourceFinalNewline := textLines(before)
	result := make([]string, 0, len(source))
	sourcePosition := 0
	finalNewline := sourceFinalNewline
	for hunkIndex, hunk := range patch.hunks {
		start := hunkPosition(hunk.oldStart, hunk.oldCount)
		if start < sourcePosition || start > len(source) {
			return nil, fmt.Errorf("hunk %d starts outside or before the previous hunk", hunkIndex+1)
		}
		result = append(result, source[sourcePosition:start]...)
		sourcePosition = start
		newStart := hunkPosition(hunk.newStart, hunk.newCount)
		if newStart != len(result) {
			return nil, fmt.Errorf("hunk %d new-file position does not follow the previous hunk", hunkIndex+1)
		}
		var lastNewLine *patchLine
		for lineIndex := range hunk.lines {
			line := &hunk.lines[lineIndex]
			switch line.kind {
			case ' ':
				if sourcePosition >= len(source) || source[sourcePosition] != line.text {
					return nil, fmt.Errorf("hunk %d context does not match at source line %d", hunkIndex+1, sourcePosition+1)
				}
				result = append(result, line.text)
				sourcePosition++
				lastNewLine = line
			case '-':
				if sourcePosition >= len(source) || source[sourcePosition] != line.text {
					return nil, fmt.Errorf("hunk %d deletion does not match at source line %d", hunkIndex+1, sourcePosition+1)
				}
				sourcePosition++
			case '+':
				result = append(result, line.text)
				lastNewLine = line
			}
		}
		if sourcePosition == len(source) && lastNewLine != nil {
			finalNewline = !lastNewLine.noNewline
		} else if sourcePosition == len(source) {
			finalNewline = len(result) > 0
		}
	}
	result = append(result, source[sourcePosition:]...)
	if len(result) == 0 {
		return nil, nil
	}
	encoded := []byte(strings.Join(result, "\n"))
	if finalNewline || (patch.kind == changeAdd && len(patch.hunks) == 0) {
		encoded = append(encoded, '\n')
	}
	return encoded, nil
}

func hunkPosition(start, count int) int {
	if count == 0 {
		return start
	}
	return start - 1
}

func textLines(value []byte) ([]string, bool) {
	if len(value) == 0 {
		return nil, false
	}
	text := string(value)
	finalNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if finalNewline {
		lines = lines[:len(lines)-1]
	}
	return lines, finalNewline
}

func (tool *applyPatchTool) commit(ctx context.Context, operations []operation) error {
	committed := make([]operation, 0, len(operations))
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rollback(tool.workspace, committed))
		}
		if err := verifyCurrent(tool.workspace, operation, tool.maxFileBytes); err != nil {
			return errors.Join(err, rollback(tool.workspace, committed))
		}
		var err error
		if operation.kind == changeDelete {
			err = tool.workspace.Remove(operation.path)
		} else {
			err = tool.workspace.AtomicWrite(operation.path, operation.after, operation.mode)
		}
		if err != nil {
			return errors.Join(fmt.Errorf("commit patch for %q: %w", operation.path, err), rollback(tool.workspace, committed))
		}
		committed = append(committed, operation)
	}
	return nil
}

func verifyCurrent(scoped *workspace.Workspace, operation operation, maxFileBytes int64) error {
	if operation.kind == changeAdd {
		if _, err := scoped.Lstat(operation.path); err == nil {
			return fmt.Errorf("patch target %q appeared after validation", operation.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("verify patch target %q: %w", operation.path, err)
		}
		return nil
	}
	document, err := readDocument(scoped, operation.path, maxFileBytes)
	if err != nil {
		return fmt.Errorf("verify patch preimage %q: %w", operation.path, err)
	}
	if document.Digest != operation.beforeDigest {
		return fmt.Errorf("patch preimage %q changed after validation", operation.path)
	}
	return nil
}

func rollback(scoped *workspace.Workspace, operations []operation) error {
	var rollbackErrors []error
	for index := len(operations) - 1; index >= 0; index-- {
		operation := operations[index]
		var err error
		if operation.kind == changeAdd {
			err = scoped.Remove(operation.path)
		} else {
			err = scoped.AtomicWrite(operation.path, operation.before, operation.mode)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback %q: %w", operation.path, err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func readDocument(scoped *workspace.Workspace, path string, maxFileBytes int64) (filetext.Document, error) {
	file, err := scoped.Open(path)
	if err != nil {
		return filetext.Document{}, err
	}
	document, readErr := filetext.Read(file, maxFileBytes)
	closeErr := file.Close()
	return document, errors.Join(readErr, closeErr)
}

type patchResponse struct {
	Changes []patchChange `json:"changes"`
}

type patchChange struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	BeforeDigest string `json:"before_digest,omitempty"`
	AfterDigest  string `json:"after_digest,omitempty"`
	Bytes        int    `json:"bytes"`
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
