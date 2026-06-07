package attachment

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/tools"
)

type Options struct {
	CWD string
}

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

func Path(path string) Attachment {
	return Attachment{kind: attachmentPath, path: path}
}

func PathWithName(path, name string) Attachment {
	return Attachment{kind: attachmentPath, path: path, name: name}
}

func Bytes(data []byte, mediaType, name string) Attachment {
	return Attachment{kind: attachmentBytes, data: append([]byte(nil), data...), mediaType: mediaType, name: name}
}

func Text(text, name string) Attachment {
	return Attachment{kind: attachmentText, text: text, name: name}
}

func Build(items []Attachment, opts Options) ([]message.Block, error) {
	blocks := make([]message.Block, 0, len(items))
	for _, item := range items {
		block, err := build(item, opts)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func build(item Attachment, opts Options) (message.Block, error) {
	switch item.kind {
	case attachmentPath:
		return buildPath(item, opts)
	case attachmentBytes:
		return buildBytes(item)
	case attachmentText:
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

func buildPath(item Attachment, opts Options) (message.Block, error) {
	resolved := tools.ResolvePath(item.path, opts.CWD)
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
	if mimeType := tools.DetectImageMIMEType(resolved); mimeType != "" {
		return message.ImageBlock(base64.StdEncoding.EncodeToString(data), mimeType, attachmentName(item, resolved), nil), nil
	}
	if tools.DetectDocumentMIMEType(resolved) == "application/pdf" {
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

func textBlock(text, name string) message.Block {
	return message.TextBlock(
		fmt.Sprintf("<file name=\"%s\">\n%s\n</file>", message.EscapeXMLAttr(name), text),
		map[string]any{"attachment": true, "path": name},
	)
}
