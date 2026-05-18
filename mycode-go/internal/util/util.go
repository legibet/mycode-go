// Package util holds small helpers shared by multiple internal packages.
//
// Each helper here exists because at least two packages need byte-identical
// behavior; if a helper is only used by one package, it belongs in that
// package, not here.
package util

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RandomHex16 returns a 16-byte random identifier as a 32-char hex string.
// Falls back to a unix-nano-derived id if crypto/rand fails (extremely rare,
// but the callers must not panic).
func RandomHex16() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// ExpandAbs resolves "~" to the user's home directory and returns the
// cleaned absolute form of path. Returns "" for empty input.
func ExpandAbs(path string) string {
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

// ResolveSymlinks returns an absolute path with symlinks resolved for every
// existing path component. Missing trailing components are kept as written.
func ResolveSymlinks(path string) string {
	path = ExpandAbs(path)
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

// xmlAttrEscaper handles the four characters that must be escaped inside an
// XML attribute value when we serialize attachments into prompt text.
var xmlAttrEscaper = strings.NewReplacer(
	"&", "&amp;",
	`"`, "&quot;",
	"<", "&lt;",
	">", "&gt;",
)

// EscapeXMLAttr escapes the four reserved characters for XML attribute values.
func EscapeXMLAttr(value string) string {
	return xmlAttrEscaper.Replace(value)
}
