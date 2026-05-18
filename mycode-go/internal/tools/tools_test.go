package tools

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

var png1x1 = mustBase64Decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j1X8AAAAASUVORK5CYII=")

func TestTruncateText(t *testing.T) {
	t.Run("by lines", func(t *testing.T) {
		text := strings.Join([]string{
			"line 0",
			"line 1",
			"line 2",
			"line 3",
		}, "\n")
		content, trunc := TruncateText(text, 2, 1024, false)
		if content != "line 0\nline 1" {
			t.Fatalf("unexpected content: %q", content)
		}
		if !trunc.Truncated || trunc.TruncatedBy != "lines" {
			t.Fatalf("unexpected truncation: %#v", trunc)
		}
	})

	t.Run("tail", func(t *testing.T) {
		text := strings.Join([]string{
			"line 0",
			"line 1",
			"line 2",
			"line 3",
		}, "\n")
		content, trunc := TruncateText(text, 2, 1024, true)
		if content != "line 2\nline 3" {
			t.Fatalf("unexpected content: %q", content)
		}
		if !trunc.Truncated || trunc.TruncatedBy != "lines" {
			t.Fatalf("unexpected truncation: %#v", trunc)
		}
	})

	t.Run("single oversized line", func(t *testing.T) {
		content, trunc := TruncateText(strings.Repeat("x", 1000), 100, 100, false)
		if len(content) == 0 || len(content) > 100 {
			t.Fatalf("unexpected content length: %d", len(content))
		}
		if !trunc.Truncated || trunc.TruncatedBy != "bytes" || trunc.OutputLines != 1 {
			t.Fatalf("unexpected truncation: %#v", trunc)
		}
	})
}

func TestDetectImageMIMETypeFallsBackToExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.png")
	if err := osWriteFile(path, nil); err != nil {
		t.Fatal(err)
	}
	if got := DetectImageMIMEType(path); got != "image/png" {
		t.Fatalf("unexpected mime type: %q", got)
	}
}

func TestResolvePathExpandsHomeAndResolvesSymlinks(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("home unavailable: %v", err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		resolvedHome = filepath.Clean(home)
	}
	if got := ResolvePath("~", "/"); got != resolvedHome {
		t.Fatalf("unexpected home path: %q, want %q", got, resolvedHome)
	}

	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	linkDir := filepath.Join(dir, "link")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolvedRealDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ResolvePath("link/new.txt", dir), filepath.Join(resolvedRealDir, "new.txt"); got != want {
		t.Fatalf("unexpected symlink path: %q, want %q", got, want)
	}
}

func TestRead(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.Read(".", 0, 0)
		if !result.IsError || !strings.Contains(strings.ToLower(result.Output), "not a file") {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("invalid utf8", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "binary.bin")
		if err := osWriteFile(path, []byte{0x80, 0x81, 0x82}); err != nil {
			t.Fatal(err)
		}
		result := executor.Read("binary.bin", 0, 0)
		if !result.IsError || !strings.Contains(strings.ToLower(result.Output), "utf-8") {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("long line adds hint", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "long.txt")
		data := "short\n" + strings.Repeat("x", ReadMaxLineChars+10)
		if err := osWriteFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
		result := executor.Read("long.txt", 0, 0)
		if !strings.Contains(result.Output, "... [line truncated]") {
			t.Fatalf("missing truncation marker: %q", result.Output)
		}
		if !strings.Contains(result.Output, "sed -n '2p'") {
			t.Fatalf("missing hint: %q", result.Output)
		}
	})

	t.Run("unicode long line stays utf8", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "unicode.txt")
		data := strings.Repeat("你", ReadMaxLineChars+10)
		if err := osWriteFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
		result := executor.Read("unicode.txt", 0, 0)
		if result.IsError || !utf8.ValidString(result.Output) || !strings.Contains(result.Output, "... [line truncated]") {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("image content", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, true)
		path := filepath.Join(dir, "tiny.png")
		if err := osWriteFile(path, png1x1); err != nil {
			t.Fatal(err)
		}
		result := executor.Read("tiny.png", 0, 0)
		if result.IsError || result.Output != "Read image file [image/png]" {
			t.Fatalf("unexpected result: %#v", result)
		}
		if len(result.Content) != 2 || result.Content[1].Type != "image" {
			t.Fatalf("unexpected content: %#v", result.Content)
		}
	})
}

func TestEdit(t *testing.T) {
	t.Run("closest hint", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "test.txt")
		if err := osWriteFile(path, []byte("alpha\nbeta gamma\ndelta\n")); err != nil {
			t.Fatal(err)
		}
		result := executor.Edit("test.txt", []map[string]string{{
			"oldText": "beta gamam",
			"newText": "replacement",
		}})
		if !result.IsError || !strings.Contains(result.Output, "closest line: beta gamma") {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("fuzzy trailing whitespace", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "test.py")
		if err := osWriteFile(path, []byte("def f():\n    return 1    \n")); err != nil {
			t.Fatal(err)
		}
		result := executor.Edit("test.py", []map[string]string{{
			"oldText": "def f():\n    return 1\n",
			"newText": "def f():\n    return 2\n",
		}})
		assertEditOK(t, result)
		data, err := osReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "def f():\n    return 2\n" {
			t.Fatalf("unexpected file: %q", string(data))
		}
	})

	t.Run("fuzzy crlf", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "test.txt")
		if err := osWriteFile(path, []byte("line1\r\nline2\r\n")); err != nil {
			t.Fatal(err)
		}
		result := executor.Edit("test.txt", []map[string]string{{
			"oldText": "line1\nline2\n",
			"newText": "line1\nlineX\n",
		}})
		assertEditOK(t, result)
		data, err := osReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "line1\r\nlineX\r\n" {
			t.Fatalf("unexpected file: %q", string(data))
		}
	})

	t.Run("normalization ambiguity", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "test.txt")
		if err := osWriteFile(path, []byte("x  \r\nx\t\r\n")); err != nil {
			t.Fatal(err)
		}
		result := executor.Edit("test.txt", []map[string]string{{
			"oldText": "x\n",
			"newText": "y\n",
		}})
		if !result.IsError || !strings.Contains(strings.ToLower(result.Output), "after normalization") {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("multi edit line expansion", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "test.txt")
		if err := osWriteFile(path, []byte("a\nb\nc\n")); err != nil {
			t.Fatal(err)
		}
		result := executor.Edit("test.txt", []map[string]string{
			{"oldText": "a", "newText": "a1\na2"},
			{"oldText": "c", "newText": "C"},
		})
		assertEditOK(t, result)
		data, err := osReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "a1\na2\nb\nC\n" {
			t.Fatalf("unexpected file: %q", string(data))
		}
	})

	t.Run("multi edit patch uses file order", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		path := filepath.Join(dir, "test.txt")
		if err := osWriteFile(path, []byte("line 1\nline 2\nline 3\nline 4\nline 5\n")); err != nil {
			t.Fatal(err)
		}
		result := executor.Edit("test.txt", []map[string]string{
			{"oldText": "line 5", "newText": "LINE 5"},
			{"oldText": "line 2", "newText": "LINE 2"},
		})
		assertEditOK(t, result)
		patch := result.Metadata["patch"].(string)
		if strings.Index(patch, "-line 2") >= strings.Index(patch, "-line 5") {
			t.Fatalf("unexpected patch order:\n%s", patch)
		}
	})

	t.Run("line stats", func(t *testing.T) {
		cases := []struct {
			name    string
			initial string
			oldText string
			newText string
			added   int
			removed int
		}{
			{name: "replace", initial: "foo\n", oldText: "foo", newText: "bar", added: 1, removed: 1},
			{name: "insert", initial: "foo\n", oldText: "foo", newText: "foo\nbar\nbaz", added: 2, removed: 0},
			{name: "delete", initial: "a\nb\nc\n", oldText: "a\nb\nc", newText: "a\nc", added: 0, removed: 1},
			{name: "multiline replace", initial: "x\nold1\nold2\ny\n", oldText: "old1\nold2", newText: "new1\nnew2\nnew3", added: 3, removed: 2},
			{name: "reorder", initial: "A\nB\n", oldText: "A\nB", newText: "B\nA", added: 1, removed: 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				executor := NewExecutor(dir, dir, false)
				path := filepath.Join(dir, "test.txt")
				if err := osWriteFile(path, []byte(tc.initial)); err != nil {
					t.Fatal(err)
				}
				result := executor.Edit("test.txt", []map[string]string{{
					"oldText": tc.oldText,
					"newText": tc.newText,
				}})
				assertEditOK(t, result)
				if result.Metadata["added_lines"] != tc.added || result.Metadata["removed_lines"] != tc.removed {
					t.Fatalf("unexpected stats: %#v", result.Metadata)
				}
			})
		}
	})
}

func TestBash(t *testing.T) {
	t.Run("empty output", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.Bash("empty", "true", 0, nil)
		if result.Output != "(empty)" {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("nonzero exit is not tool error", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.Bash("exit", "echo some output; exit 42", 0, nil)
		if result.IsError {
			t.Fatalf("unexpected error result: %#v", result)
		}
		if !strings.Contains(result.Output, "some output") || !strings.Contains(result.Output, "[exit code: 42]") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
	})

	t.Run("large output truncates and saves log", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.Bash("large", "seq 1 3000", 0, nil)
		if !strings.Contains(result.Output, "Truncated:") || !strings.Contains(result.Output, "Full output:") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		if _, err := osReadFile(filepath.Join(dir, "tool-output", "bash-large.log")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("long single line truncates by bytes", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.Bash("long-line", "head -c 60000 /dev/zero | tr '\\000' x", 0, nil)
		if !strings.Contains(result.Output, "KB limit") || !strings.Contains(result.Output, "Full output:") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		if strings.Contains(result.Output, "0 lines") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.Bash("timeout", "sleep 2", 1, nil)
		if !result.IsError || !strings.Contains(strings.ToLower(result.Output), "timeout") {
			t.Fatalf("unexpected result: %#v", result)
		}
	})
}

func assertEditOK(t *testing.T, result Result) {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", result)
	}
	if _, ok := result.Metadata["patch"].(string); !ok {
		t.Fatalf("missing patch metadata: %#v", result.Metadata)
	}
	if _, ok := result.Metadata["added_lines"].(int); !ok {
		t.Fatalf("missing added_lines metadata: %#v", result.Metadata)
	}
	if _, ok := result.Metadata["removed_lines"].(int); !ok {
		t.Fatalf("missing removed_lines metadata: %#v", result.Metadata)
	}
}

func mustBase64Decode(value string) []byte {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return data
}

func osWriteFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func osReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
