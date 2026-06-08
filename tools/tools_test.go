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

func TestRead(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.read(".", 0, 0)
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
		result := executor.read("binary.bin", 0, 0)
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
		result := executor.read("long.txt", 0, 0)
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
		result := executor.read("unicode.txt", 0, 0)
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
		result := executor.read("tiny.png", 0, 0)
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
		result := executor.edit("test.txt", []editEntry{{
			OldText: "beta gamam",
			NewText: "replacement",
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
		result := executor.edit("test.py", []editEntry{{
			OldText: "def f():\n    return 1\n",
			NewText: "def f():\n    return 2\n",
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
		result := executor.edit("test.txt", []editEntry{{
			OldText: "line1\nline2\n",
			NewText: "line1\nlineX\n",
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
		result := executor.edit("test.txt", []editEntry{{
			OldText: "x\n",
			NewText: "y\n",
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
		result := executor.edit("test.txt", []editEntry{
			{OldText: "a", NewText: "a1\na2"},
			{OldText: "c", NewText: "C"},
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
		result := executor.edit("test.txt", []editEntry{
			{OldText: "line 5", NewText: "LINE 5"},
			{OldText: "line 2", NewText: "LINE 2"},
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
				result := executor.edit("test.txt", []editEntry{{
					OldText: tc.oldText,
					NewText: tc.newText,
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
		result := executor.bash("empty", "true", 0, nil)
		if result.Output != "(empty)" {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("nonzero exit is not tool error", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.bash("exit", "echo some output; exit 42", 0, nil)
		if result.IsError {
			t.Fatalf("unexpected error result: %#v", result)
		}
		if !strings.Contains(result.Output, "some output") || !strings.Contains(result.Output, "[exit code: 42]") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
	})

	t.Run("does not wait for implicit stdin", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.bash("stdin-devnull", `python3 -c "import sys; print(repr(sys.stdin.read()))"`, 1, nil)
		if result.Output != "''" {
			t.Fatalf("unexpected output: %q", result.Output)
		}
	})

	t.Run("large output truncates and saves log", func(t *testing.T) {
		dir := t.TempDir()
		executor := NewExecutor(dir, dir, false)
		result := executor.bash("large", "seq 1 3000", 0, nil)
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
		result := executor.bash("long-line", "head -c 60000 /dev/zero | tr '\\000' x", 0, nil)
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
		result := executor.bash("timeout", "sleep 2", 1, nil)
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

// TestBuiltinToolSchemas locks the reflected schema shape against drift:
// optional fields stay out of "required", nested entries inline, and the
// JSON Schema dialect marker is stripped.
func TestBuiltinToolSchemas(t *testing.T) {
	read := Read.InputSchema
	if read["type"] != "object" || read["additionalProperties"] != false {
		t.Fatalf("read schema shape: %#v", read)
	}
	if _, ok := read["$schema"]; ok {
		t.Fatalf("dialect marker should be stripped: %#v", read)
	}
	if req := requiredNames(read); len(req) != 1 || !req["path"] {
		t.Fatalf("read required = %v, want only path", req)
	}

	properties, _ := Edit.InputSchema["properties"].(map[string]any)
	edits, _ := properties["edits"].(map[string]any)
	items, _ := edits["items"].(map[string]any)
	if items["type"] != "object" {
		t.Fatalf("edit items should inline an object: %#v", edits)
	}
	if req := requiredNames(items); !req["oldText"] || !req["newText"] {
		t.Fatalf("edit item required = %v, want oldText and newText", req)
	}
}

func requiredNames(schema map[string]any) map[string]bool {
	out := map[string]bool{}
	req, _ := schema["required"].([]any)
	for _, name := range req {
		if text, ok := name.(string); ok {
			out[text] = true
		}
	}
	return out
}
