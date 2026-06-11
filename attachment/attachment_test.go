package attachment_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/legibet/mycode-go/attachment"
)

func TestResolvePathExpandsHomeAndResolvesSymlinks(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("home unavailable: %v", err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		resolvedHome = filepath.Clean(home)
	}
	if got := attachment.ResolvePath("~", "/"); got != resolvedHome {
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
	if got, want := attachment.ResolvePath("link/new.txt", dir), filepath.Join(resolvedRealDir, "new.txt"); got != want {
		t.Fatalf("unexpected symlink path: %q, want %q", got, want)
	}
}

func TestBuildPathTextAttachment(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, `report <"draft">.txt`), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, err := attachment.Build([]attachment.Attachment{
		attachment.Path(`report <"draft">.txt`),
	}, cwd)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	block := blocks[0]
	want := "<file name=\"report &lt;&quot;draft&quot;&gt;.txt\">\nhello\n\n</file>"
	if block.Type != "text" || block.Text != want {
		t.Fatalf("unexpected block: %#v", block)
	}
	if block.Meta["attachment"] != true || block.Meta["path"] != `report <"draft">.txt` {
		t.Fatalf("unexpected meta: %#v", block.Meta)
	}
}

func TestBuildPathImageAttachment(t *testing.T) {
	cwd := t.TempDir()
	data := mustDecodeBase64(t, "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j1X8AAAAASUVORK5CYII=")
	if err := os.WriteFile(filepath.Join(cwd, "tiny.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, err := attachment.Build([]attachment.Attachment{
		attachment.Path("tiny.png"),
	}, cwd)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	block := blocks[0]
	if block.Type != "image" || block.Data != base64.StdEncoding.EncodeToString(data) || block.MIMEType != "image/png" || block.Name != "tiny.png" {
		t.Fatalf("unexpected image block: %#v", block)
	}
}

func TestBuildPathPDFAttachment(t *testing.T) {
	cwd := t.TempDir()
	data := []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
	if err := os.WriteFile(filepath.Join(cwd, "report.pdf"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, err := attachment.Build([]attachment.Attachment{
		attachment.Path("report.pdf"),
	}, cwd)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	block := blocks[0]
	if block.Type != "document" || block.Data != base64.StdEncoding.EncodeToString(data) || block.MIMEType != "application/pdf" || block.Name != "report.pdf" {
		t.Fatalf("unexpected document block: %#v", block)
	}
}

func TestBuildBytesImageAttachment(t *testing.T) {
	data := mustDecodeBase64(t, "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j1X8AAAAASUVORK5CYII=")

	blocks, err := attachment.Build([]attachment.Attachment{
		attachment.Bytes(data, "image/png", "inline.png"),
	}, "")
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	block := blocks[0]
	if block.Type != "image" || block.Data != base64.StdEncoding.EncodeToString(data) || block.MIMEType != "image/png" || block.Name != "inline.png" {
		t.Fatalf("unexpected image block: %#v", block)
	}
}

func TestBuildTextAttachment(t *testing.T) {
	blocks, err := attachment.Build([]attachment.Attachment{
		attachment.Text("package main\n", `main <"v2">.go`),
	}, "")
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	block := blocks[0]
	want := "<file name=\"main &lt;&quot;v2&quot;&gt;.go\">\npackage main\n\n</file>"
	if block.Type != "text" || block.Text != want {
		t.Fatalf("unexpected text block: %#v", block)
	}
	if block.Meta["attachment"] != true || block.Meta["path"] != `main <"v2">.go` {
		t.Fatalf("unexpected meta: %#v", block.Meta)
	}
}

func TestBuildTextAttachmentRequiresName(t *testing.T) {
	_, err := attachment.Build([]attachment.Attachment{
		attachment.Text("package main\n", ""),
	}, "")
	if err == nil {
		t.Fatal("expected text attachment name error")
	}
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
