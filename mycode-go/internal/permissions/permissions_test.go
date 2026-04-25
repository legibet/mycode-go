package permissions

import (
	"path/filepath"
	"testing"

	"github.com/legibet/mycode-go/internal/config"
)

func TestDecisionFor(t *testing.T) {
	cases := []struct {
		name       string
		permission config.PermissionConfig
		tier       Tier
		want       Decision
	}{
		{"safe allows write edits", config.PermissionConfig{Level: "safe", Mode: "ask"}, TierSafe, DecisionAllow},
		{"safe asks for standard shell", config.PermissionConfig{Level: "safe", Mode: "ask"}, TierStandard, DecisionAsk},
		{"deny mode denies outside level", config.PermissionConfig{Level: "readonly", Mode: "deny"}, TierSafe, DecisionDeny},
		{"yolo allows everything", config.PermissionConfig{Level: "yolo", Mode: "deny"}, TierYolo, DecisionAllow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecisionFor(tc.permission, tc.tier); got != tc.want {
				t.Fatalf("DecisionFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyFileTools(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	skillRoot := filepath.Join(root, "skills", "example")

	cases := []struct {
		name      string
		toolName  string
		path      string
		skillRoot []string
		want      Tier
	}{
		{"workspace read", "read", "README.md", nil, TierReadonly},
		{"workspace edit", "edit", "README.md", nil, TierSafe},
		{"outside read", "read", filepath.Join(root, "outside.txt"), nil, TierYolo},
		{"skill read", "read", filepath.Join(skillRoot, "SKILL.md"), []string{skillRoot}, TierReadonly},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := ClassifyTool(tc.toolName, map[string]any{"path": tc.path}, cwd, tc.skillRoot)
			if check.Tier != tc.want {
				t.Fatalf("ClassifyTool() = %#v, want tier %q", check, tc.want)
			}
		})
	}
}

func TestClassifyBash(t *testing.T) {
	cases := []struct {
		command string
		want    Tier
	}{
		{"ls -la", TierReadonly},
		{"git status --short", TierReadonly},
		{"git diff -- src/app.py", TierReadonly},
		{"git -C repo status --short", TierReadonly},
		{"git branch --show-current", TierReadonly},
		{"rg TODO src", TierReadonly},
		{"find src -name '*.py'", TierReadonly},
		{"command -v pytest", TierReadonly},
		{"uv run pytest tests", TierStandard},
		{"uv run ruff check", TierStandard},
		{"pnpm --dir web test:run", TierStandard},
		{"npm run build", TierStandard},
		{"python -m pytest", TierStandard},
		{"go test ./...", TierStandard},
		{"cargo check", TierStandard},
		{"just check", TierStandard},
		{"uv sync --dev", TierStandard},
		{"pnpm install", TierStandard},
		{"awk '{print $1}' file.txt", TierStandard},
		{"sed 's/foo/bar/' file.txt", TierStandard},
		{"rm -rf dist", TierYolo},
		{"git reset --hard HEAD", TierYolo},
		{"sed -i s/a/b/ file.txt", TierYolo},
		{"find . -name x -delete", TierYolo},
		{"find . -name x -exec rm {} ;", TierYolo},
		{"find . -name x -execdir touch out ;", TierYolo},
		{"find . -name x -ok rm {} ;", TierYolo},
		{"find . -name x -okdir rm {} ;", TierYolo},
		{"find . -fprint /tmp/out", TierYolo},
		{"find . -fprint0 /tmp/out", TierYolo},
		{"find . -fprintf /tmp/out %p\\n", TierYolo},
		{"find . -fls /tmp/out", TierYolo},
		{"ls\npwd", TierYolo},
		{"sleep 1 &", TierYolo},
		{"ls && rm -rf dist", TierYolo},
		{"grep foo a.txt | wc -l", TierYolo},
		{"echo hi > out.txt", TierYolo},
		{"echo hi 2>&1", TierYolo},
		{"echo $(date)", TierYolo},
		{"git push --force", TierYolo},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			check := ClassifyTool("bash", map[string]any{"command": tc.command}, ".", nil)
			if check.Tier != tc.want {
				t.Fatalf("ClassifyTool() = %#v, want tier %q", check, tc.want)
			}
		})
	}
}
