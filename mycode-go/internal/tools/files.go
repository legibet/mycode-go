package tools

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/legibet/mycode-go/internal/message"
)

// ResolvePath resolves a path relative to cwd.
func ResolvePath(path, cwd string) string {
	expanded := path
	if strings.HasPrefix(expanded, "~/") {
		home, _ := os.UserHomeDir()
		expanded = filepath.Join(home, strings.TrimPrefix(expanded, "~/"))
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded)
	}
	return filepath.Clean(filepath.Join(cwd, expanded))
}

// DetectImageMIMEType returns a supported image type.
func DetectImageMIMEType(path string) string {
	return detectImageMIMEType(path, readFileHeader(path, 16))
}

func detectImageMIMEType(path string, header []byte) string {
	switch {
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(header, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(header, []byte("GIF87a")), bytes.HasPrefix(header, []byte("GIF89a")):
		return "image/gif"
	case len(header) >= 12 && bytes.HasPrefix(header, []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")):
		return "image/webp"
	}

	switch mt := mime.TypeByExtension(filepath.Ext(path)); mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mt
	default:
		return ""
	}
}

// DetectDocumentMIMEType returns a supported document type.
func DetectDocumentMIMEType(path string) string {
	return detectDocumentMIMEType(path, readFileHeader(path, 5))
}

func detectDocumentMIMEType(path string, header []byte) string {
	if bytes.HasPrefix(header, []byte("%PDF-")) {
		return "application/pdf"
	}
	if mime.TypeByExtension(filepath.Ext(path)) == "application/pdf" {
		return "application/pdf"
	}
	return ""
}

func readFileHeader(path string, size int) []byte {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() {
		_ = file.Close()
	}()

	header := make([]byte, size)
	n, err := file.Read(header)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return header[:n]
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
func (e *Executor) Read(path string, offset, limit int) Result {
	filePath := ResolvePath(path, e.cwd)

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

	data, err := os.ReadFile(filePath)
	if err != nil {
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	if imageType := detectImageMIMEType(filePath, data); imageType != "" {
		if !e.supportsImageInput {
			return errorResult("error: image input is not supported by the current model")
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
	if !utf8.Valid(data) {
		return errorResult("error: file is not valid utf-8 text: " + path)
	}

	startLine := 1
	if offset > 0 {
		startLine = offset
	}
	lineLimit := DefaultMaxLines
	if limit > 0 {
		lineLimit = limit
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	lines := []string{}
	totalLines := 0
	nextOffset := 0
	firstShortened := 0
	shortenedLines := 0

	for scanner.Scan() {
		totalLines++
		if totalLines < startLine {
			continue
		}
		if len(lines) >= lineLimit {
			nextOffset = totalLines
			break
		}
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if len(line) > ReadMaxLineChars {
			if firstShortened == 0 {
				firstShortened = totalLines
			}
			shortenedLines++
			line = line[:ReadMaxLineChars] + " ... [line truncated]"
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	if totalLines < startLine && (totalLines != 0 || startLine != 1) {
		return errorResult(fmt.Sprintf("error: offset %d beyond end of file (%d lines)", offset, totalLines))
	}

	parts := []string{}
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
func (e *Executor) Write(path, content string) Result {
	filePath := ResolvePath(path, e.cwd)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return errorResult(fmt.Sprintf("error: failed to write file: %v", err))
	}
	if err := atomicWriteText(filePath, content, ""); err != nil {
		return errorResult(fmt.Sprintf("error: failed to write file: %v", err))
	}
	return Result{Output: "Wrote " + path}
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

func trimTailLines(lines []string) []string {
	if len(lines) <= DefaultMaxLines {
		return lines
	}
	return slices.Clone(lines[len(lines)-DefaultMaxLines:])
}

func splitLineCount(text string) int {
	if text == "" {
		return 0
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.TrimSuffix(normalized, "\n")
	normalized = strings.TrimSuffix(normalized, "\r")
	if normalized == "" {
		return 1
	}
	return len(strings.Split(normalized, "\n"))
}

func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'"
}

// MIMEByExtension returns a MIME type using extension fallback.
func MIMEByExtension(path string) string {
	return mime.TypeByExtension(filepath.Ext(path))
}
