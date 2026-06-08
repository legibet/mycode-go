// Package tools implements the four built-in agent tools: read, write, edit,
// bash. The set is fixed; new capabilities live in skills.
package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/invopop/jsonschema"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/legibet/mycode-go/attachment"
	"github.com/legibet/mycode-go/message"
)

const (
	DefaultMaxLines      = 2000
	DefaultMaxBytes      = 50 * 1024
	ReadMaxLineChars     = 2000
	BashTimeout          = 120 * time.Second
	BashMaxInMemoryBytes = 5_000_000
)

// Spec is the provider-facing tool definition plus its runner. Build custom
// tools with Define; the built-in tools are package-level Spec values
// (Read, Write, Edit, Bash).
type Spec struct {
	Name          string                                 `json:"name"`
	Description   string                                 `json:"description"`
	InputSchema   map[string]any                         `json:"input_schema"`
	StreamsOutput bool                                   `json:"-"`
	Runner        func(context.Context, ToolCall) Result `json:"-"`
}

type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
	CWD   string
	Emit  OutputCallback
	// exec is the built-in tool engine, injected by Executor.Run so built-in
	// tool runners can reach file/shell behavior and cancellation.
	exec *Executor
}

// Call carries the decoded, typed input for a tool defined with Define, plus
// the runtime handles the handler may need. Emit is only meaningful for specs
// created with WithStreaming.
type Call[T any] struct {
	ID    string
	Input T
	CWD   string
	Emit  func(string)
}

// Option configures a Spec built by Define.
type Option func(*Spec)

// WithStreaming marks a tool as emitting incremental output via Call.Emit.
func WithStreaming() Option {
	return func(spec *Spec) { spec.StreamsOutput = true }
}

// Define builds a Spec from a typed handler. The provider input schema is
// reflected from T; at call time the raw input map is decoded into T before run
// is invoked. It is the single entry point for custom tools.
func Define[T any](name, description string, run func(context.Context, Call[T]) Result, opts ...Option) Spec {
	spec := Spec{
		Name:        name,
		Description: description,
		InputSchema: reflectSchema[T](),
		Runner: func(ctx context.Context, call ToolCall) Result {
			input, err := decodeInput[T](call.Input)
			if err != nil {
				return invalidInput(err)
			}
			return run(ctx, Call[T]{ID: call.ID, Input: input, CWD: call.CWD, Emit: call.Emit})
		},
	}
	for _, opt := range opts {
		opt(&spec)
	}
	return spec
}

// decodeInput converts a raw tool input map into the typed args struct T.
func decodeInput[T any](input map[string]any) (T, error) {
	var out T
	if len(input) == 0 {
		return out, nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(data, &out)
}

func invalidInput(err error) Result {
	return Result{Output: "error: invalid tool input: " + err.Error(), IsError: true}
}

// schemaReflector inlines definitions so tool schemas are flat objects with
// additionalProperties:false, matching what providers expect.
var schemaReflector = jsonschema.Reflector{
	DoNotReference: true,
	ExpandedStruct: true,
	Anonymous:      true,
}

// reflectSchema builds the provider-facing JSON schema for a tool input type,
// stripping the JSON Schema dialect marker providers do not need.
func reflectSchema[T any]() map[string]any {
	var zero T
	data, err := json.Marshal(schemaReflector.Reflect(zero))
	if err != nil {
		panic("tools: marshal reflected schema: " + err.Error())
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic("tools: decode reflected schema: " + err.Error())
	}
	delete(out, "$schema")
	delete(out, "$id")
	return out
}

type (
	BeforeToolHook func(context.Context, ToolCall) (Result, bool)
	AfterToolHook  func(context.Context, ToolCall, Result) (Result, bool)
)

type Hooks struct {
	BeforeTool []BeforeToolHook
	AfterTool  []AfterToolHook
}

// Result is the canonical tool result. Output is provider-facing text;
// Content carries optional structured blocks (e.g. inline images);
// Metadata carries optional structured info for the UI (e.g. edit patches).
type Result struct {
	Output   string          `json:"output"`
	Content  []message.Block `json:"content,omitempty"`
	Metadata map[string]any  `json:"metadata,omitempty"`
	IsError  bool            `json:"is_error"`
}

// Truncation reports whether and why text output was shortened.
type Truncation struct {
	Truncated   bool
	TruncatedBy string
	OutputLines int
	OutputBytes int
}

// OutputCallback receives live bash output one line at a time.
type OutputCallback func(text string)

// Executor runs the four built-in tools for one session. It tracks live bash
// children so Cancel kills the whole tree on user-requested cancellation.
type Executor struct {
	cwd                string
	toolOutputDir      string
	supportsImageInput bool
	mu                 sync.Mutex
	activeCmds         map[*exec.Cmd]struct{}
}

// Built-in tool input types. Field tags drive the reflected provider schema:
// json names the wire key, omitempty marks an optional field, and
// jsonschema_description carries the field description verbatim.
type readArgs struct {
	Path   string `json:"path" jsonschema_description:"File path (relative or absolute)."`
	Offset int    `json:"offset,omitempty" jsonschema_description:"Line number to start from (1-indexed)."`
	Limit  int    `json:"limit,omitempty" jsonschema_description:"Maximum number of lines to return."`
}

type writeArgs struct {
	Path    string `json:"path" jsonschema_description:"File path (relative or absolute)."`
	Content string `json:"content" jsonschema_description:"File content."`
}

type editEntry struct {
	OldText string `json:"oldText" jsonschema_description:"Exact text to find (must be unique in the file)."`
	NewText string `json:"newText" jsonschema_description:"Replacement text."`
}

type editArgs struct {
	Path  string      `json:"path" jsonschema_description:"File path (relative or absolute)."`
	Edits []editEntry `json:"edits" jsonschema_description:"Replacements to apply. All matched against the original file, not incrementally."`
}

type bashArgs struct {
	Command string `json:"command" jsonschema_description:"Shell command."`
	Timeout int    `json:"timeout,omitempty" jsonschema_description:"Timeout in seconds (optional)."`
}

// Built-in tools. List any subset in agent.Config.Tools. Their runners reach
// the file/shell engine through the executor injected by Executor.Run. The
// descriptions are what the model sees and must match actual behavior.
var (
	Read = Spec{
		Name:        "read",
		Description: "Read a UTF-8 text file or supported image file. Returns up to 2000 lines for text files. Use offset/limit for large files. Very long lines are shortened.",
		InputSchema: reflectSchema[readArgs](),
		Runner: func(_ context.Context, call ToolCall) Result {
			args, err := decodeInput[readArgs](call.Input)
			if err != nil {
				return invalidInput(err)
			}
			return call.exec.read(args.Path, args.Offset, args.Limit)
		},
	}
	Write = Spec{
		Name:        "write",
		Description: "Write a file (create or overwrite).",
		InputSchema: reflectSchema[writeArgs](),
		Runner: func(_ context.Context, call ToolCall) Result {
			args, err := decodeInput[writeArgs](call.Input)
			if err != nil {
				return invalidInput(err)
			}
			return call.exec.write(args.Path, args.Content)
		},
	}
	Edit = Spec{
		Name: "edit",
		Description: "Edit a file by replacing text snippets. " +
			"Each edits[].oldText must match uniquely in the original file. " +
			"For multiple disjoint changes in one file, use one call with multiple edits.",
		InputSchema: reflectSchema[editArgs](),
		Runner: func(_ context.Context, call ToolCall) Result {
			args, err := decodeInput[editArgs](call.Input)
			if err != nil {
				return invalidInput(err)
			}
			return call.exec.edit(args.Path, args.Edits)
		},
	}
	Bash = Spec{
		Name:          "bash",
		Description:   "Run a shell command in the session working directory. Large output returns the tail and saves the full log to a file.",
		StreamsOutput: true,
		InputSchema:   reflectSchema[bashArgs](),
		Runner: func(_ context.Context, call ToolCall) Result {
			args, err := decodeInput[bashArgs](call.Input)
			if err != nil {
				return invalidInput(err)
			}
			return call.exec.bash(call.ID, args.Command, args.Timeout, call.Emit)
		},
	}
)

// NewExecutor roots the executor at cwd. Bash logs spill into
// sessionDir/tool-output when output exceeds BashMaxInMemoryBytes.
func NewExecutor(cwd, sessionDir string, supportsImageInput bool) *Executor {
	absoluteCWD := cwd
	if absoluteCWD != "" {
		if resolved, err := filepath.Abs(cwd); err == nil {
			absoluteCWD = resolved
		}
	}
	if sessionDir == "" {
		sessionDir = absoluteCWD
	}
	return &Executor{
		cwd:                absoluteCWD,
		toolOutputDir:      filepath.Join(sessionDir, "tool-output"),
		supportsImageInput: supportsImageInput,
		activeCmds:         map[*exec.Cmd]struct{}{},
	}
}

// CancelActive kills all bash commands started by this executor.
func (e *Executor) CancelActive() {
	e.mu.Lock()
	cmds := slices.Collect(maps.Keys(e.activeCmds))
	clear(e.activeCmds)
	e.mu.Unlock()

	for _, cmd := range cmds {
		killCmdTree(cmd)
	}
}

func (e *Executor) trackCmd(cmd *exec.Cmd) {
	e.mu.Lock()
	e.activeCmds[cmd] = struct{}{}
	e.mu.Unlock()
}

func (e *Executor) untrackCmd(cmd *exec.Cmd) {
	e.mu.Lock()
	delete(e.activeCmds, cmd)
	e.mu.Unlock()
}

// killCmdTree kills the whole process group so spawned children
// (e.g. compiler subprocesses under `make`) don't leak as orphans.
func killCmdTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// ParseToolArguments parses the JSON object providers stream as tool call
// arguments. Empty input yields an empty map.
func ParseToolArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	return object, nil
}

func errorResult(output string) Result {
	return Result{Output: output, IsError: true}
}

// TruncateText truncates text by line and byte budget.
func TruncateText(text string, maxLines, maxBytes int, tail bool) (string, Truncation) {
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	lines := strings.Split(text, "\n")
	totalBytes := len(text)
	source := lines
	if tail {
		source = slices.Clone(lines)
		slices.Reverse(source)
	}

	out := make([]string, 0, min(maxLines, len(lines)))
	outputBytes := 0
	for _, line := range source {
		if len(out) >= maxLines {
			break
		}
		lineBytes := len(line) + 1
		if outputBytes+lineBytes > maxBytes {
			break
		}
		out = append(out, line)
		outputBytes += lineBytes
	}

	if tail {
		slices.Reverse(out)
	}

	if len(out) == 0 && len(lines) > 0 {
		target := lines[0]
		if tail {
			target = lines[len(lines)-1]
		}
		encoded := []byte(target)
		if len(encoded) > maxBytes {
			if tail {
				encoded = encoded[len(encoded)-maxBytes:]
			} else {
				encoded = encoded[:maxBytes]
			}
		}
		content := string(bytes.ToValidUTF8(encoded, nil))
		return content, Truncation{
			Truncated:   true,
			TruncatedBy: "bytes",
			OutputLines: 1,
			OutputBytes: len(encoded),
		}
	}

	content := strings.Join(out, "\n")
	truncated := len(out) < len(lines) || outputBytes < totalBytes
	truncatedBy := ""
	if truncated {
		if len(out) < len(lines) {
			if len(out) == maxLines {
				truncatedBy = "lines"
			} else {
				truncatedBy = "bytes"
			}
		} else {
			truncatedBy = "bytes"
		}
	}

	return content, Truncation{
		Truncated:   truncated,
		TruncatedBy: truncatedBy,
		OutputLines: len(out),
		OutputBytes: outputBytes,
	}
}

// Read reads a text file or image.
// Run executes spec with this executor injected as the call's runtime context,
// so built-in tool runners can reach file/shell behavior and cancellation.
func (e *Executor) Run(ctx context.Context, spec Spec, id string, input map[string]any, emit OutputCallback) Result {
	return spec.Runner(ctx, ToolCall{ID: id, Name: spec.Name, Input: input, CWD: e.cwd, Emit: emit, exec: e})
}

func (e *Executor) read(path string, offset, limit int) Result {
	filePath := attachment.ResolvePath(path, e.cwd)

	info, err := os.Stat(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errorResult("error: file not found: " + path)
		}
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	if info.IsDir() {
		return errorResult("error: not a file: " + path)
	}

	if imageType := attachment.DetectImageMIMEType(filePath); imageType != "" {
		if !e.supportsImageInput {
			return errorResult("error: image input is not supported by the current model")
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
		}
		summary := "Read image file [" + imageType + "]"
		return Result{
			Output: summary,
			Content: []message.Block{
				message.TextBlock(summary, nil),
				message.ImageBlock(base64.StdEncoding.EncodeToString(data), imageType, filepath.Base(filePath), nil),
			},
		}
	}

	startLine := 1
	if offset > 0 {
		startLine = offset
	}
	lineLimit := DefaultMaxLines
	if limit > 0 {
		lineLimit = limit
	}

	file, err := os.Open(filePath)
	if err != nil {
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReader(file)
	var lines []string
	totalLines := 0
	nextOffset := 0
	firstShortened := 0
	shortenedLines := 0

	for {
		rawLine, err := reader.ReadBytes('\n')
		if len(rawLine) > 0 {
			if !utf8.Valid(rawLine) {
				return errorResult("error: file is not valid utf-8 text: " + path)
			}
			totalLines++
			if totalLines >= startLine {
				if len(lines) >= lineLimit {
					nextOffset = totalLines
					break
				}
				line := strings.TrimRight(string(rawLine), "\r\n")
				if len(line) > ReadMaxLineChars {
					runes := []rune(line)
					if len(runes) <= ReadMaxLineChars {
						lines = append(lines, line)
						continue
					}
					if firstShortened == 0 {
						firstShortened = totalLines
					}
					shortenedLines++
					line = string(runes[:ReadMaxLineChars]) + " ... [line truncated]"
				}
				lines = append(lines, line)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
		}
	}
	if totalLines < startLine && (totalLines != 0 || startLine != 1) {
		return errorResult(fmt.Sprintf("error: offset %d beyond end of file (%d lines)", offset, totalLines))
	}

	var parts []string
	content := strings.Join(lines, "\n")
	if content != "" {
		parts = append(parts, content)
	}
	if nextOffset > 0 {
		parts = append(parts, fmt.Sprintf("[Showing lines %d-%d. Use offset=%d to continue.]", startLine, nextOffset-1, nextOffset))
	}
	if firstShortened > 0 {
		prefix := fmt.Sprintf("[Line %d was shortened to %d chars.", firstShortened, ReadMaxLineChars)
		if shortenedLines > 1 {
			prefix = fmt.Sprintf("[%d lines were shortened to %d chars. First shortened line: %d.", shortenedLines, ReadMaxLineChars, firstShortened)
		}
		parts = append(parts, prefix+
			"\nUse bash to inspect it in bytes:\n"+
			fmt.Sprintf("sed -n '%dp' %s | head -c 2000\n", firstShortened, shellQuote(filePath))+
			fmt.Sprintf("sed -n '%dp' %s | tail -c +2001 | head -c 2000]", firstShortened, shellQuote(filePath)))
	}

	content = strings.Join(parts, "\n\n")
	return Result{Output: content}
}

// Write writes a file atomically.
func (e *Executor) write(path, content string) Result {
	filePath := attachment.ResolvePath(path, e.cwd)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return errorResult(fmt.Sprintf("error: failed to write file: %v", err))
	}
	if err := atomicWriteText(filePath, content, ""); err != nil {
		return errorResult(fmt.Sprintf("error: failed to write file: %v", err))
	}
	return Result{Output: "Wrote " + path}
}

type editSpec struct {
	oldText string
	newText string
	prefix  string
	index   int
}

type editMatch struct {
	start   int
	end     int
	newText string
	index   int
}

// Edit applies one or more replacements against the original file content.
func (e *Executor) edit(path string, edits []editEntry) Result {
	if len(edits) == 0 {
		return errorResult("error: edits must not be empty")
	}

	filePath := attachment.ResolvePath(path, e.cwd)
	info, err := os.Stat(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errorResult("error: file not found: " + path)
		}
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	if info.IsDir() {
		return errorResult("error: not a file: " + path)
	}

	multi := len(edits) > 1
	items := make([]editSpec, 0, len(edits))
	for i, edit := range edits {
		oldText := edit.OldText
		newText := edit.NewText
		prefix := ""
		if multi {
			prefix = fmt.Sprintf("edits[%d]: ", i)
		}
		if oldText == "" {
			return errorResult("error: " + prefix + "oldText must not be empty")
		}
		if oldText == newText {
			return errorResult("error: " + prefix + "oldText and newText are identical")
		}
		items = append(items, editSpec{oldText: oldText, newText: newText, prefix: prefix, index: i})
	}

	readMTime := info.ModTime().UnixNano()
	data, err := os.ReadFile(filePath)
	if err != nil {
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	text := string(data)
	newline := ""
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}

	matches := make([]editMatch, 0, len(items))
	var normalizedText string
	var indexMap []int
	normalizedLoaded := false

	for _, edit := range items {
		exactCount := strings.Count(text, edit.oldText)
		if exactCount > 1 {
			return errorResult(fmt.Sprintf("error: %soldText occurs %d times; provide a more specific oldText", edit.prefix, exactCount))
		}
		if exactCount == 1 {
			start := strings.Index(text, edit.oldText)
			matches = append(matches, editMatch{
				start:   start,
				end:     start + len(edit.oldText),
				newText: edit.newText,
				index:   edit.index,
			})
			continue
		}

		if !normalizedLoaded {
			normalizedText, indexMap = normalizeText(text)
			normalizedLoaded = true
		}
		normalizedOld, _ := normalizeText(edit.oldText)
		normalizedCount := strings.Count(normalizedText, normalizedOld)
		if normalizedCount > 1 {
			return errorResult(fmt.Sprintf("error: %soldText occurs %d times after normalization; provide a more specific oldText", edit.prefix, normalizedCount))
		}
		if normalizedCount == 0 {
			modelText := "error: " + edit.prefix + "oldText not found"
			if hint := closestLineHint(text, edit.oldText); hint != "" {
				modelText += ". closest line: " + hint
			}
			return errorResult(modelText)
		}

		start := strings.Index(normalizedText, normalizedOld)
		end := start + len(normalizedOld)
		origStart := indexMap[start]
		origEnd := len(text)
		if end < len(indexMap) {
			origEnd = indexMap[end]
		}
		matches = append(matches, editMatch{
			start:   origStart,
			end:     origEnd,
			newText: edit.newText,
			index:   edit.index,
		})
	}

	slices.SortFunc(matches, func(a, b editMatch) int {
		return cmp.Compare(a.start, b.start)
	})
	for i := 1; i < len(matches); i++ {
		if matches[i-1].end > matches[i].start {
			return errorResult(fmt.Sprintf("error: edits[%d] and edits[%d] overlap", matches[i-1].index, matches[i].index))
		}
	}

	updated := text
	for _, match := range slices.Backward(matches) {
		updated = updated[:match.start] + match.newText + updated[match.end:]
	}
	if updated == text {
		return errorResult("error: edits produced no changes")
	}

	patch, addedLines, removedLines, err := buildEditPatch(path, text, updated)
	if err != nil {
		return errorResult(fmt.Sprintf("error: failed to build edit patch: %v", err))
	}

	summary := "Updated " + path
	if len(items) > 1 {
		summary = fmt.Sprintf("Updated %s (%d edits)", path, len(items))
	}

	infoAfterRead, err := os.Stat(filePath)
	if err == nil && infoAfterRead.ModTime().UnixNano() != readMTime {
		return errorResult("error: file changed while editing; read it again and retry")
	}
	if err := atomicWriteText(filePath, updated, newline); err != nil {
		return errorResult(fmt.Sprintf("error: failed to write file: %v", err))
	}

	return Result{
		Output: summary,
		Metadata: map[string]any{
			"patch":         patch,
			"added_lines":   addedLines,
			"removed_lines": removedLines,
		},
	}
}

// Bash runs a shell command and streams output.
func (e *Executor) bash(toolCallID, command string, timeoutSeconds int, onOutput OutputCallback) Result {
	timeout := int(BashTimeout / time.Second)
	if timeoutSeconds > 0 {
		timeout = timeoutSeconds
	}

	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = e.cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer

	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return errorResult("error: " + err.Error())
	}
	waited := false
	killed := false
	e.trackCmd(cmd)
	stopReading := make(chan struct{})
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
		e.untrackCmd(cmd)
		if !waited && !killed {
			killCmdTree(cmd)
		}
	}()
	defer close(stopReading)

	lines := make(chan string, 256)
	readerErrors := make(chan error, 1)
	go func() {
		defer close(lines)
		buffered := bufio.NewReader(reader)
		for {
			line, err := buffered.ReadString('\n')
			if len(line) > 0 {
				select {
				case lines <- strings.TrimRight(line, "\n"):
				case <-stopReading:
					return
				}
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				select {
				case readerErrors <- err:
				case <-stopReading:
				}
				return
			}
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
		_ = writer.Close()
	}()

	logPath := filepath.Join(e.toolOutputDir, "bash-"+toolCallID+".log")
	var keptLines []string
	keptBytes := 0
	totalLineCount := 0
	var tailLines []string
	var logFile *os.File
	var savedOutputPath string
	doneReading := false
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()

	for !doneReading {
		select {
		case <-timer.C:
			killCmdTree(cmd)
			killed = true
			if logFile != nil {
				_ = logFile.Close()
			}
			return Result{Output: fmt.Sprintf("error: timeout after %ds", timeout), IsError: true}
		case err := <-readerErrors:
			if logFile != nil {
				_ = logFile.Close()
			}
			return errorResult("error: " + err.Error())
		case line, ok := <-lines:
			if !ok {
				doneReading = true
				continue
			}
			totalLineCount++
			keptBytes += len(line) + 1

			if logFile == nil {
				keptLines = append(keptLines, line)
				if keptBytes > BashMaxInMemoryBytes {
					if err := os.MkdirAll(e.toolOutputDir, 0o755); err == nil {
						file, fileErr := os.Create(logPath)
						if fileErr == nil {
							logFile = file
							savedOutputPath = logPath
							if len(keptLines) > 0 {
								_, _ = io.WriteString(logFile, strings.Join(keptLines, "\n"))
								_, _ = io.WriteString(logFile, "\n")
								tailLines = append(tailLines, keptLines...)
								if len(tailLines) > DefaultMaxLines {
									tailLines = append([]string(nil), tailLines[len(tailLines)-DefaultMaxLines:]...)
								}
							}
							keptLines = nil
						}
					}
				}
			} else {
				tailLines = append(tailLines, line)
				if len(tailLines) > DefaultMaxLines {
					tailLines = append([]string(nil), tailLines[len(tailLines)-DefaultMaxLines:]...)
				}
				_, _ = io.WriteString(logFile, line)
				_, _ = io.WriteString(logFile, "\n")
			}

			if onOutput != nil {
				onOutput(line)
			}
		}
	}

	waitErr := <-waitDone
	waited = true
	if logFile != nil {
		_ = logFile.Close()
	}
	if waitErr != nil {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			return Result{Output: "error: cancelled", IsError: true}
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok && exitErr.ExitCode() < 0 {
			return Result{Output: "error: cancelled", IsError: true}
		}
	}

	rawOutput := strings.Join(keptLines, "\n")
	if logFile != nil || len(tailLines) > 0 {
		rawOutput = strings.Join(tailLines, "\n")
	}
	output := strings.TrimSpace(rawOutput)
	if output == "" {
		output = "(empty)"
	}
	content, trunc := TruncateText(output, DefaultMaxLines, DefaultMaxBytes, true)
	if savedOutputPath == "" && trunc.Truncated {
		if err := os.MkdirAll(e.toolOutputDir, 0o755); err == nil {
			if err := os.WriteFile(logPath, []byte(rawOutput), 0o644); err == nil {
				savedOutputPath = logPath
			}
		}
	}

	result := content
	wasTruncated := savedOutputPath != "" || trunc.Truncated
	if wasTruncated {
		notice := ""
		if trunc.TruncatedBy == "bytes" {
			if totalLineCount <= 1 {
				notice = fmt.Sprintf("[Truncated: showing last %dKB of output (%dKB limit).", DefaultMaxBytes/1024, DefaultMaxBytes/1024)
			} else {
				notice = fmt.Sprintf("[Truncated: showing tail output (%dKB limit).", DefaultMaxBytes/1024)
			}
		} else {
			notice = fmt.Sprintf("[Truncated: last %d of %d lines.", trunc.OutputLines, totalLineCount)
		}
		if savedOutputPath != "" {
			notice += " Full output: " + savedOutputPath + "]"
		} else {
			notice += "]"
		}
		result += "\n\n" + notice
	}

	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0 {
		result += fmt.Sprintf("\n\n[exit code: %d]", cmd.ProcessState.ExitCode())
	}
	return Result{Output: result}
}

func atomicWriteText(path, content, newline string) error {
	tmp := path + ".tmp"
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if newline == "\r\n" {
		normalized = strings.ReplaceAll(normalized, "\n", "\r\n")
	}
	if err := os.WriteFile(tmp, []byte(normalized), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'"
}

func buildEditPatch(path, original, updated string) (string, int, int, error) {
	patch, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(original),
		B:        difflib.SplitLines(updated),
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
		Context:  3,
		Eol:      "\n",
	})
	if err != nil {
		return "", 0, 0, err
	}

	addedLines := 0
	removedLines := 0
	for i, line := range strings.Split(patch, "\n") {
		if i < 2 {
			continue
		}
		if strings.HasPrefix(line, "+") {
			addedLines++
		} else if strings.HasPrefix(line, "-") {
			removedLines++
		}
	}
	return patch, addedLines, removedLines, nil
}

func closestLineHint(text, needle string) string {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return ""
	}

	bestRatio := 0.0
	bestLine := ""
	for line := range strings.SplitSeq(text, "\n") {
		candidate := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if candidate == "" {
			continue
		}
		ratio := similarityRatio(needle, candidate)
		if ratio > bestRatio {
			bestRatio = ratio
			bestLine = candidate
			if ratio >= 1 {
				break
			}
		}
	}
	if bestRatio < 0.6 || bestLine == "" {
		return ""
	}
	if len(bestLine) > 120 {
		return bestLine[:117] + "..."
	}
	return bestLine
}

func similarityRatio(a, b string) float64 {
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 && len(br) == 0 {
		return 1
	}
	if len(ar) == 0 || len(br) == 0 {
		return 0
	}

	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 0
			if ar[i-1] != br[j-1] {
				cost = 1
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		copy(prev, curr)
	}
	maxLen := max(len(ar), len(br))
	return 1 - float64(prev[len(br)])/float64(maxLen)
}

func normalizeText(text string) (string, []int) {
	var builder strings.Builder
	indexMap := make([]int, 0, len(text))
	pos := 0
	for _, line := range splitLinesKeepEnds(text) {
		content := strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimRight(content, " \t")
		builder.WriteString(trimmed)
		for i := range len(trimmed) {
			indexMap = append(indexMap, pos+i)
		}
		if len(content) != len(line) {
			builder.WriteByte('\n')
			indexMap = append(indexMap, pos+len(content))
		}
		pos += len(line)
	}
	return builder.String(), indexMap
}

func splitLinesKeepEnds(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for start < len(text) {
		end := start
		for end < len(text) && text[end] != '\n' && text[end] != '\r' {
			end++
		}
		if end == len(text) {
			lines = append(lines, text[start:end])
			break
		}
		if text[end] == '\r' && end+1 < len(text) && text[end+1] == '\n' {
			end += 2
		} else {
			end++
		}
		lines = append(lines, text[start:end])
		start = end
	}
	return lines
}
