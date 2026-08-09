// Package filesystem provides read-only Tools scoped to one Workspace
package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extensions/internal/filetext"
	"github.com/qed-runtime/qed/internal/jsonstrict"
	"github.com/qed-runtime/qed/workspace"
)

const (
	// SearchTextToolName is the model-facing name of the text search Tool
	SearchTextToolName = "search_text"
	// ReadFileToolName is the model-facing name of the bounded file read Tool
	ReadFileToolName = "read_file"

	defaultMaxFileBytes     = 4 << 20
	defaultMaxOutputBytes   = 1 << 20
	defaultMaxSearchFiles   = 10000
	defaultMaxSearchResults = 200
	defaultMaxReadLines     = 2000
	maximumArgumentBytes    = 64 << 10
	maximumMatchTextBytes   = 4096
)

// Options bounds filesystem Tool resource use
type Options struct {
	MaxFileBytes     int64
	MaxOutputBytes   int
	MaxSearchFiles   int
	MaxSearchResults int
	MaxReadLines     int
}

// NewTools constructs search_text and read_file for one Workspace
func NewTools(scoped *workspace.Workspace, options Options) ([]agent.Tool, error) {
	if scoped == nil {
		return nil, errors.New("filesystem Workspace is required")
	}
	configured, err := configuredOptions(options)
	if err != nil {
		return nil, err
	}
	return []agent.Tool{
		&searchTextTool{workspace: scoped, options: configured},
		&readFileTool{workspace: scoped, options: configured},
	}, nil
}

type configuredFilesystemOptions struct {
	maxFileBytes     int64
	maxOutputBytes   int
	maxSearchFiles   int
	maxSearchResults int
	maxReadLines     int
}

func configuredOptions(options Options) (configuredFilesystemOptions, error) {
	configured := configuredFilesystemOptions{
		maxFileBytes:     valueOrDefault64(options.MaxFileBytes, defaultMaxFileBytes),
		maxOutputBytes:   valueOrDefault(options.MaxOutputBytes, defaultMaxOutputBytes),
		maxSearchFiles:   valueOrDefault(options.MaxSearchFiles, defaultMaxSearchFiles),
		maxSearchResults: valueOrDefault(options.MaxSearchResults, defaultMaxSearchResults),
		maxReadLines:     valueOrDefault(options.MaxReadLines, defaultMaxReadLines),
	}
	if configured.maxFileBytes <= 0 || configured.maxOutputBytes <= 0 || configured.maxSearchFiles <= 0 ||
		configured.maxSearchResults <= 0 || configured.maxReadLines <= 0 {
		return configuredFilesystemOptions{}, errors.New("filesystem Tool limits must be positive")
	}
	return configured, nil
}

type searchTextTool struct {
	workspace *workspace.Workspace
	options   configuredFilesystemOptions
}

func (tool *searchTextTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         SearchTextToolName,
		Description:  "Search bounded UTF-8 files within the workspace and return structured matches",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}},"mode":{"type":"string","enum":["literal","regexp"]},"case_sensitive":{"type":"boolean"},"max_results":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}`),
		Capabilities: []string{string(capability.FilesystemRead)},
	}
}

func (tool *searchTextTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct {
		Query         string   `json:"query"`
		Paths         []string `json:"paths,omitempty"`
		Mode          string   `json:"mode,omitempty"`
		CaseSensitive *bool    `json:"case_sensitive,omitempty"`
		MaxResults    int      `json:"max_results,omitempty"`
	}
	if err := jsonstrict.Decode(call.Arguments, maximumArgumentBytes, &input); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode search_text arguments: %w", err)
	}
	if input.Query == "" {
		return agent.ToolResult{}, errors.New("search_text query is required")
	}
	if input.Mode == "" {
		input.Mode = "literal"
	}
	if input.Mode != "literal" && input.Mode != "regexp" {
		return agent.ToolResult{}, fmt.Errorf("unsupported search_text mode %q", input.Mode)
	}
	caseSensitive := true
	if input.CaseSensitive != nil {
		caseSensitive = *input.CaseSensitive
	}
	maxResults := input.MaxResults
	if maxResults == 0 {
		maxResults = tool.options.maxSearchResults
	}
	if maxResults < 1 || maxResults > tool.options.maxSearchResults {
		return agent.ToolResult{}, fmt.Errorf("search_text max_results must be between 1 and %d", tool.options.maxSearchResults)
	}
	if len(input.Paths) == 0 {
		input.Paths = []string{"."}
	}

	matcher, err := newMatcher(input.Query, input.Mode, caseSensitive)
	if err != nil {
		return agent.ToolResult{}, err
	}
	release := tool.workspace.AcquireRead()
	defer release()

	files, discoveryTruncated, err := tool.discoverFiles(ctx, input.Paths)
	if err != nil {
		return agent.ToolResult{}, err
	}
	response := searchResponse{Truncated: discoveryTruncated}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return agent.ToolResult{}, err
		}
		opened, err := tool.workspace.Open(file.relative)
		if err != nil {
			response.SkippedFiles++
			continue
		}
		document, readErr := filetext.Read(opened, tool.options.maxFileBytes)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			response.SkippedFiles++
			continue
		}
		response.SearchedFiles++
		lines := splitLines(string(document.Data))
		for lineIndex, line := range lines {
			indices := matcher(line, maxResults-len(response.Matches))
			for _, index := range indices {
				text, lineTruncated := truncateUTF8(line, maximumMatchTextBytes)
				response.Matches = append(response.Matches, searchMatch{
					Path:          file.relative,
					Line:          lineIndex + 1,
					Column:        utf8.RuneCountInString(line[:index]) + 1,
					Text:          text,
					LineTruncated: lineTruncated,
				})
				if len(response.Matches) == maxResults {
					response.Truncated = true
					break
				}
			}
			if len(response.Matches) == maxResults {
				break
			}
		}
		if len(response.Matches) == maxResults {
			break
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode search_text result: %w", err)
	}
	if len(encoded) > tool.options.maxOutputBytes {
		return agent.ToolResult{}, errors.New("search_text result exceeds the output limit")
	}
	return agent.ToolResult{Output: string(encoded)}, nil
}

type discoveredFile struct {
	relative string
}

func (tool *searchTextTool) discoverFiles(ctx context.Context, paths []string) ([]discoveredFile, bool, error) {
	unique := make(map[string]discoveredFile)
	truncated := false
	for _, requested := range paths {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		resolved, err := tool.workspace.Resolve(requested)
		if err != nil {
			return nil, false, fmt.Errorf("resolve search_text path %q: %w", requested, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, false, err
		}
		if info.Mode().IsRegular() {
			relative, err := tool.workspace.Relative(resolved)
			if err != nil {
				return nil, false, err
			}
			unique[relative] = discoveredFile{relative: relative}
			continue
		}
		if !info.IsDir() {
			return nil, false, fmt.Errorf("search_text path %q must be a regular file or directory", requested)
		}
		err = filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				if path != resolved && entry.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return nil
			}
			relative, err := tool.workspace.Relative(path)
			if err != nil {
				return err
			}
			unique[relative] = discoveredFile{relative: relative}
			if len(unique) >= tool.options.maxSearchFiles {
				truncated = true
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return nil, false, fmt.Errorf("walk search_text path %q: %w", requested, err)
		}
		if truncated {
			break
		}
	}
	files := make([]discoveredFile, 0, len(unique))
	for _, file := range unique {
		files = append(files, file)
	}
	sort.Slice(files, func(first, second int) bool { return files[first].relative < files[second].relative })
	return files, truncated, nil
}

type readFileTool struct {
	workspace *workspace.Workspace
	options   configuredFilesystemOptions
}

func (tool *readFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         ReadFileToolName,
		Description:  "Read a bounded line range from one UTF-8 workspace file and return its digest",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`),
		Capabilities: []string{string(capability.FilesystemRead)},
	}
}

func (tool *readFileTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line,omitempty"`
		EndLine   int    `json:"end_line,omitempty"`
	}
	if err := jsonstrict.Decode(call.Arguments, maximumArgumentBytes, &input); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode read_file arguments: %w", err)
	}
	if input.Path == "" {
		return agent.ToolResult{}, errors.New("read_file path is required")
	}
	if input.StartLine == 0 {
		input.StartLine = 1
	}
	if input.StartLine < 1 || input.EndLine < 0 {
		return agent.ToolResult{}, errors.New("read_file line numbers must be positive")
	}
	if input.EndLine != 0 && input.EndLine < input.StartLine {
		return agent.ToolResult{}, errors.New("read_file end_line must not precede start_line")
	}

	release := tool.workspace.AcquireRead()
	defer release()
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	if _, err := tool.workspace.ResolveFile(input.Path); err != nil {
		return agent.ToolResult{}, err
	}
	opened, err := tool.workspace.Open(input.Path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	document, readErr := filetext.Read(opened, tool.options.maxFileBytes)
	closeErr := opened.Close()
	if readErr != nil {
		return agent.ToolResult{}, fmt.Errorf("read_file %q: %w", input.Path, readErr)
	}
	if closeErr != nil {
		return agent.ToolResult{}, fmt.Errorf("close read_file %q: %w", input.Path, closeErr)
	}
	lines := splitLines(string(document.Data))
	if len(lines) == 0 {
		response := readResponse{Path: filepath.ToSlash(filepath.Clean(input.Path)), Digest: document.Digest, StartLine: 1}
		encoded, err := json.Marshal(response)
		if err != nil {
			return agent.ToolResult{}, err
		}
		if len(encoded) > tool.options.maxOutputBytes {
			return agent.ToolResult{}, errors.New("read_file result exceeds the output limit")
		}
		return agent.ToolResult{Output: string(encoded)}, nil
	}
	if input.StartLine > len(lines) {
		return agent.ToolResult{}, fmt.Errorf("read_file start_line %d exceeds total lines %d", input.StartLine, len(lines))
	}
	end := input.EndLine
	if end == 0 {
		end = input.StartLine + tool.options.maxReadLines - 1
		if end > len(lines) {
			end = len(lines)
		}
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end-input.StartLine+1 > tool.options.maxReadLines {
		return agent.ToolResult{}, fmt.Errorf("read_file range exceeds %d lines", tool.options.maxReadLines)
	}
	content := strings.Join(lines[input.StartLine-1:end], "\n")
	if end < len(lines) || strings.HasSuffix(string(document.Data), "\n") {
		content += "\n"
	}
	if len(content) > tool.options.maxOutputBytes {
		return agent.ToolResult{}, errors.New("read_file content exceeds the output limit, request a smaller line range")
	}
	response := readResponse{
		Path:       filepath.ToSlash(filepath.Clean(input.Path)),
		Digest:     document.Digest,
		StartLine:  input.StartLine,
		EndLine:    end,
		TotalLines: len(lines),
		Content:    content,
		Truncated:  input.StartLine > 1 || end < len(lines),
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode read_file result: %w", err)
	}
	if len(encoded) > tool.options.maxOutputBytes {
		return agent.ToolResult{}, errors.New("read_file result exceeds the output limit, request a smaller line range")
	}
	return agent.ToolResult{Output: string(encoded)}, nil
}

type searchResponse struct {
	Matches       []searchMatch `json:"matches"`
	SearchedFiles int           `json:"searched_files"`
	SkippedFiles  int           `json:"skipped_files,omitempty"`
	Truncated     bool          `json:"truncated"`
}

type searchMatch struct {
	Path          string `json:"path"`
	Line          int    `json:"line"`
	Column        int    `json:"column"`
	Text          string `json:"text"`
	LineTruncated bool   `json:"line_truncated,omitempty"`
}

type readResponse struct {
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line,omitempty"`
	TotalLines int    `json:"total_lines"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`
}

type lineMatcher func(string, int) []int

func newMatcher(query, mode string, caseSensitive bool) (lineMatcher, error) {
	if mode == "regexp" {
		pattern := query
		if !caseSensitive {
			pattern = "(?i:" + pattern + ")"
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile search_text regexp: %w", err)
		}
		return func(line string, limit int) []int {
			if limit <= 0 {
				return nil
			}
			locations := compiled.FindAllStringIndex(line, limit)
			indices := make([]int, len(locations))
			for index := range locations {
				indices[index] = locations[index][0]
			}
			return indices
		}, nil
	}
	if !caseSensitive {
		compiled := regexp.MustCompile("(?i:" + regexp.QuoteMeta(query) + ")")
		return func(line string, limit int) []int {
			if limit <= 0 {
				return nil
			}
			locations := compiled.FindAllStringIndex(line, limit)
			indices := make([]int, len(locations))
			for index := range locations {
				indices[index] = locations[index][0]
			}
			return indices
		}, nil
	}
	needle := query
	return func(line string, limit int) []int {
		if limit <= 0 {
			return nil
		}
		haystack := line
		var indices []int
		for offset := 0; offset <= len(haystack)-len(needle) && len(indices) < limit; {
			found := strings.Index(haystack[offset:], needle)
			if found < 0 {
				break
			}
			position := offset + found
			indices = append(indices, position)
			offset = position + len(needle)
		}
		return indices
	}, nil
}

func splitLines(value string) []string {
	if value == "" {
		return nil
	}
	lines := strings.Split(value, "\n")
	if strings.HasSuffix(value, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func truncateUTF8(value string, maximum int) (string, bool) {
	if len(value) <= maximum {
		return value, false
	}
	end := maximum
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], true
}

func valueOrDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func valueOrDefault64(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}
