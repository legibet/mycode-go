package attachment

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/legibet/mycode-go/message"
)

// Attachment is one pending input item; convert to message blocks with Build.
// Construct with Path, PathWithName, Bytes, or Text.
type Attachment struct {
	kind      attachmentKind
	path      string
	data      []byte
	text      string
	mediaType string
	name      string
}

type attachmentKind int

const (
	attachmentPath attachmentKind = iota + 1
	attachmentBytes
	attachmentText
)

// Path attaches the file at path: images and PDFs become media blocks, UTF-8
// text becomes a <file> text block. Relative paths resolve against Build's cwd.
func Path(path string) Attachment {
	return Attachment{kind: attachmentPath, path: path}
}

// PathWithName is Path with an explicit display name instead of the basename.
func PathWithName(path, name string) Attachment {
	return Attachment{kind: attachmentPath, path: path, name: name}
}

// Bytes attaches raw image/* or application/pdf data without touching disk.
func Bytes(data []byte, mediaType, name string) Attachment {
	return Attachment{kind: attachmentBytes, data: append([]byte(nil), data...), mediaType: mediaType, name: name}
}

// Text attaches an inline snippet as a named <file> text block.
func Text(text, name string) Attachment {
	return Attachment{kind: attachmentText, text: text, name: name}
}

// Build converts attachments to message blocks in order, resolving relative
// paths against cwd.
func Build(items []Attachment, cwd string) ([]message.Block, error) {
	blocks := make([]message.Block, 0, len(items))
	for _, item := range items {
		block, err := build(item, cwd)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func build(item Attachment, cwd string) (message.Block, error) {
	switch item.kind {
	case attachmentPath:
		return buildPath(item, cwd)
	case attachmentBytes:
		return buildBytes(item)
	case attachmentText:
		if item.name == "" {
			return message.Block{}, fmt.Errorf("attachment text requires a non-empty name")
		}
		return textBlock(item.text, item.name), nil
	default:
		return message.Block{}, fmt.Errorf("unsupported attachment")
	}
}

func buildBytes(item Attachment) (message.Block, error) {
	switch item.mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return message.ImageBlock(base64.StdEncoding.EncodeToString(item.data), item.mediaType, item.name, nil), nil
	case "application/pdf":
		return message.DocumentBlock(base64.StdEncoding.EncodeToString(item.data), item.mediaType, item.name, nil), nil
	default:
		return message.Block{}, fmt.Errorf("unsupported media_type %q", item.mediaType)
	}
}

func buildPath(item Attachment, cwd string) (message.Block, error) {
	resolved := ResolvePath(item.path, cwd)
	info, err := os.Stat(resolved)
	if err != nil {
		return message.Block{}, fmt.Errorf("attachment not found: %s", item.path)
	}
	if info.IsDir() {
		return message.Block{}, fmt.Errorf("attachment is a directory: %s", item.path)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return message.Block{}, fmt.Errorf("failed to read attachment: %w", err)
	}
	if mimeType := DetectImageMIMEType(resolved); mimeType != "" {
		return message.ImageBlock(base64.StdEncoding.EncodeToString(data), mimeType, attachmentName(item, resolved), nil), nil
	}
	if DetectDocumentMIMEType(resolved) == "application/pdf" {
		return message.DocumentBlock(base64.StdEncoding.EncodeToString(data), "application/pdf", attachmentName(item, resolved), nil), nil
	}
	if !utf8.Valid(data) {
		return message.Block{}, fmt.Errorf("unsupported attachment %s: not image, PDF, or UTF-8 text", item.path)
	}

	name := attachmentName(item, resolved)
	return textBlock(string(data), name), nil
}

func attachmentName(item Attachment, resolved string) string {
	if item.name != "" {
		return item.name
	}
	return filepath.Base(resolved)
}

// ResolvePath resolves path relative to cwd, expanding "~" and resolving
// symlinks in existing path components.
func ResolvePath(path, cwd string) string {
	expanded := path
	if expanded == "~" || strings.HasPrefix(expanded, "~/") {
		expanded = expandAbs(expanded)
	}
	if filepath.IsAbs(expanded) {
		return resolveSymlinks(expanded)
	}
	return resolveSymlinks(filepath.Join(cwd, expanded))
}

func expandAbs(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	} else if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, rest)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}

func resolveSymlinks(path string) string {
	path = expandAbs(path)
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}

	sep := string(filepath.Separator)
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	current := volume
	if strings.HasPrefix(rest, sep) {
		current += sep
		rest = strings.TrimLeft(rest, sep)
	}

	parts := strings.Split(rest, sep)
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(current, part)
		resolved, err := filepath.EvalSymlinks(next)
		if err != nil {
			return filepath.Clean(filepath.Join(append([]string{current}, parts[i:]...)...))
		}
		current = resolved
	}
	return filepath.Clean(current)
}

func textBlock(text, name string) message.Block {
	return message.TextBlock(
		fmt.Sprintf("<file name=\"%s\">\n%s\n</file>", message.EscapeXMLAttr(name), text),
		map[string]any{"attachment": true, "path": name},
	)
}

// DetectImageMIMEType returns a supported image type for the file, or "".
func DetectImageMIMEType(path string) string {
	header := readFileHeader(path, 16)
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

// DetectDocumentMIMEType returns "application/pdf" for a PDF file, or "".
func DetectDocumentMIMEType(path string) string {
	header := readFileHeader(path, 5)
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
