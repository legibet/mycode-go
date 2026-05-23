package prompt

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/util"
)

const (
	maxScanDepth   = 3
	maxDirsPerRoot = 200
	nameMaxLen     = 64
)

var (
	skipDirs = map[string]struct{}{
		"node_modules": {},
		"__pycache__":  {},
		".git":         {},
	}
	namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	userHomeDir = func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return home
	}
)

const basePrompt = "" +
	"You are mycode, an expert coding assistant.\n\n" +
	"You have four tools: read, write, edit, bash.\n\n" +
	"- Use bash for file operations and exploration like `ls`, `find`, `rg`, etc.\n" +
	"- Always set offset/limit when reading large files.\n" +
	"- Always read files before editing them.\n" +
	"- Use write only for new files or complete rewrites\n" +
	"- Your response should be concise and relevant."

// Skill is the discovered skill summary injected into the system prompt.
type Skill struct {
	Name        string
	Description string
	Path        string
	Source      string
}

// Build returns the runtime system prompt.
func Build(cwd, project, home string) string {
	resolvedCWD := util.ResolveSymlinks(cwd)
	resolvedProject := util.ResolveSymlinks(project)
	parts := []string{basePrompt}
	if section := loadInstructions(resolvedCWD, resolvedProject, home); section != "" {
		parts = append(parts, section)
	}
	if section := loadSkills(resolvedCWD, resolvedProject, home); section != "" {
		parts = append(parts, section)
	}
	parts = append(parts, "Current working directory: "+resolvedCWD+"\nCurrent date: "+time.Now().Format("2006-01"))
	return strings.Join(parts, "\n\n")
}

func loadInstructions(cwd, project, home string) string {
	sections := []string{}
	for _, path := range discoverInstructionFiles(cwd, project, home) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		sections = append(sections, fmt.Sprintf("Instructions from: %s\n%s", path, text))
	}
	if len(sections) == 0 {
		return ""
	}
	return "<project_instructions>\n" +
		"Ordered from global to project to cwd; later instructions take precedence.\n\n" +
		strings.Join(sections, "\n\n") +
		"\n</project_instructions>"
}

func discoverInstructionFiles(cwd, project, home string) []string {
	resolvedCWD := util.ResolveSymlinks(cwd)
	resolvedProject := util.ResolveSymlinks(project)
	resolvedHome := util.ResolveSymlinks(home)
	files := []string{}

	globalCandidate := filepath.Join(resolvedHome, "AGENTS.md")
	compatCandidate := filepath.Join(util.ResolveSymlinks(userHomeDir()), ".agents", "AGENTS.md")
	if isFile(globalCandidate) {
		files = append(files, globalCandidate)
	} else if isFile(compatCandidate) {
		files = append(files, compatCandidate)
	}

	for _, dir := range config.ProjectDirs(resolvedCWD, resolvedProject) {
		candidate := filepath.Join(dir, "AGENTS.md")
		if isFile(candidate) {
			files = append(files, candidate)
		}
	}
	return files
}

func loadSkills(cwd, project, home string) string {
	skills := DiscoverSkills(cwd, project, home)
	if len(skills) == 0 {
		return ""
	}
	lines := []string{
		"When a task matches a skill's description, prefer it over manual alternatives - use the read tool to load the file at <location> and follow the instructions inside.",
		"Relative paths inside a skill file resolve against the skill's directory (dirname of <location>).",
		"<available_skills>",
	}
	for _, skill := range skills {
		lines = append(lines, "  <skill>")
		lines = append(lines, "    <name>"+skill.Name+"</name>")
		lines = append(lines, "    <description>"+skill.Description+"</description>")
		lines = append(lines, "    <location>"+skill.Path+"</location>")
		lines = append(lines, "  </skill>")
	}
	lines = append(lines, "</available_skills>")
	return strings.Join(lines, "\n")
}

// DiscoverSkills merges skills across global home, ~/.agents (compat),
// project AGENTS, and project .mycode/skills.
func DiscoverSkills(cwd, project, home string) []Skill {
	cwdPath := util.ResolveSymlinks(cwd)
	projectPath := util.ResolveSymlinks(project)
	homePath := util.ResolveSymlinks(home)
	compatHome := util.ResolveSymlinks(userHomeDir())

	type skillRoot struct {
		path   string
		source string
	}
	roots := []skillRoot{
		{filepath.Join(compatHome, ".agents", "skills"), "global"},
		{filepath.Join(homePath, "skills"), "global"},
	}
	for _, dir := range config.ProjectDirs(cwdPath, projectPath) {
		roots = append(roots,
			skillRoot{filepath.Join(dir, ".agents", "skills"), "project"},
			skillRoot{filepath.Join(dir, ".mycode", "skills"), "project"},
		)
	}

	// Dedupe by path, then re-key by name so a project skill overrides a
	// global one with the same name.
	skillsByName := map[string]Skill{}
	seenPaths := map[string]struct{}{}
	addSkill := func(skill Skill) {
		if _, seen := seenPaths[skill.Path]; seen {
			return
		}
		seenPaths[skill.Path] = struct{}{}
		if prev, ok := skillsByName[skill.Name]; ok {
			slog.Debug("skill overridden", "name", skill.Name, "new", skill.Path, "prev", prev.Path)
		}
		skillsByName[skill.Name] = skill
	}

	for _, root := range roots {
		for _, skill := range scanSkillRoot(root.path, root.source) {
			addSkill(skill)
		}
	}

	out := make([]Skill, 0, len(skillsByName))
	for _, name := range slices.Sorted(maps.Keys(skillsByName)) {
		out = append(out, skillsByName[name])
	}
	return out
}

func scanSkillRoot(root, source string) []Skill {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	rootEntries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	slices.SortFunc(rootEntries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})

	skills := []Skill{}
	seenPaths := map[string]struct{}{}
	addSkill := func(path, fallbackName string) {
		skill, ok := parseSkill(path, source, fallbackName)
		if !ok {
			return
		}
		if _, seen := seenPaths[skill.Path]; seen {
			return
		}
		seenPaths[skill.Path] = struct{}{}
		skills = append(skills, skill)
	}
	shouldScanDir := func(entry os.DirEntry) bool {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			return false
		}
		if _, skip := skipDirs[entry.Name()]; skip {
			return false
		}
		info, err := entry.Info()
		return err == nil && info.Mode()&os.ModeSymlink == 0
	}

	for _, entry := range rootEntries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".md" {
			addSkill(filepath.Join(root, entry.Name()), strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		}
	}

	type pendingDir struct {
		path  string
		depth int
	}
	queue := []pendingDir{}
	for _, entry := range rootEntries {
		if !shouldScanDir(entry) {
			continue
		}
		queue = append(queue, pendingDir{path: filepath.Join(root, entry.Name()), depth: 1})
	}

	dirsScanned := 0
	for len(queue) > 0 && dirsScanned < maxDirsPerRoot {
		current := queue[0]
		queue = queue[1:]
		dirsScanned++

		addSkill(filepath.Join(current.path, "SKILL.md"), filepath.Base(current.path))

		if current.depth >= maxScanDepth {
			continue
		}

		children, err := os.ReadDir(current.path)
		if err != nil {
			continue
		}
		slices.SortFunc(children, func(a, b os.DirEntry) int {
			return strings.Compare(a.Name(), b.Name())
		})
		for _, child := range children {
			if !shouldScanDir(child) {
				continue
			}
			queue = append(queue, pendingDir{
				path:  filepath.Join(current.path, child.Name()),
				depth: current.depth + 1,
			})
		}
	}

	return skills
}

func parseSkill(path, source, fallbackName string) (Skill, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}

	frontmatter, ok := parseFrontmatter(string(data))
	if !ok {
		return Skill{}, false
	}

	name := ""
	for _, candidate := range []string{asString(frontmatter["name"]), fallbackName} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || len(candidate) > nameMaxLen || !namePattern.MatchString(candidate) {
			continue
		}
		name = candidate
		break
	}
	if name == "" {
		return Skill{}, false
	}

	description := strings.TrimSpace(asString(frontmatter["description"]))
	if description == "" {
		return Skill{}, false
	}
	return Skill{
		Name:        name,
		Description: description,
		Path:        util.ExpandAbs(path),
		Source:      source,
	}, true
}

func parseFrontmatter(text string) (map[string]any, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, false
	}

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &parsed); err != nil {
		return nil, false
	}
	if parsed == nil {
		return nil, false
	}
	return parsed, true
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}
