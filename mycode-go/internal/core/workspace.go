package core

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/legibet/mycode-go/internal/config"
)

type BrowseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type BrowseResult struct {
	Root    string        `json:"root"`
	Path    string        `json:"path"`
	Current string        `json:"current"`
	Entries []BrowseEntry `json:"entries"`
	Error   string        `json:"error"`
}

func workspaceRoots() []string {
	raw := cmp.Or(
		strings.TrimSpace(os.Getenv("MYCODE_WORKSPACE_ROOTS")),
		strings.TrimSpace(os.Getenv("WORKSPACE_ROOTS")),
	)
	var values []string
	if raw != "" {
		values = strings.Split(raw, ",")
	} else {
		home, _ := os.UserHomeDir()
		values = []string{home, string(filepath.Separator)}
	}

	seen := map[string]struct{}{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		resolved := config.ResolveSymlinks(value)
		if _, err := os.Stat(resolved); err != nil {
			continue
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	if len(out) == 0 {
		cwd, _ := os.Getwd()
		out = []string{cwd}
	}
	return out
}

func browseWorkspace(root, relativePath string) BrowseResult {
	requestedRoot := config.ResolveSymlinks(root)
	if !slices.Contains(workspaceRoots(), requestedRoot) {
		return BrowseResult{Root: root, Error: "Invalid root"}
	}

	target := config.ResolveSymlinks(filepath.Join(requestedRoot, relativePath))
	if !withinWorkspaceRoot(requestedRoot, target) {
		return BrowseResult{Root: requestedRoot, Current: requestedRoot, Error: "Path outside root"}
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		return BrowseResult{Root: requestedRoot, Current: requestedRoot, Error: err.Error()}
	}
	out := make([]BrowseEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		fullPath := filepath.Join(target, entry.Name())
		rel, err := filepath.Rel(requestedRoot, fullPath)
		if err != nil {
			continue
		}
		out = append(out, BrowseEntry{Name: entry.Name(), Path: filepath.ToSlash(rel)})
	}
	slices.SortFunc(out, func(a, b BrowseEntry) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})

	relCurrent := ""
	if target != requestedRoot {
		relCurrent, err = filepath.Rel(requestedRoot, target)
		if err != nil {
			relCurrent = ""
		}
		relCurrent = filepath.ToSlash(relCurrent)
	}
	return BrowseResult{
		Root:    requestedRoot,
		Path:    relCurrent,
		Current: target,
		Entries: out,
	}
}

func withinWorkspaceRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
