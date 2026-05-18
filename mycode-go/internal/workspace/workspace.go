// Package workspace exposes the workspace browser used by the web UI.
// Roots are restricted by env vars (or default to home + /) so the API
// never lets the browser walk arbitrary host paths.
package workspace

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/legibet/mycode-go/internal/util"
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

// Roots returns the directories the UI is allowed to browse. Falls back to
// home + / when MYCODE_WORKSPACE_ROOTS / WORKSPACE_ROOTS are unset.
func Roots() []string {
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
		resolved := util.ResolveSymlinks(value)
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

// Browse returns child directories of root/relativePath. The root must be in
// Roots() and the resolved target must stay inside root.
func Browse(root, relativePath string) BrowseResult {
	requestedRoot := util.ResolveSymlinks(root)
	if !slices.Contains(Roots(), requestedRoot) {
		return BrowseResult{Root: root, Error: "Invalid root"}
	}

	target := util.ResolveSymlinks(filepath.Join(requestedRoot, relativePath))
	if !withinRoot(requestedRoot, target) {
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

func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
