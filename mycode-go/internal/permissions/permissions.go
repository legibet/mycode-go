package permissions

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/prompt"
	"github.com/legibet/mycode-go/internal/tools"
)

const (
	DeniedOutput       = "error: permission denied"
	DeniedByUserOutput = "error: permission denied by user"
)

type Tier string

const (
	TierReadonly Tier = "readonly"
	TierSafe     Tier = "safe"
	TierStandard Tier = "standard"
	TierYolo     Tier = "yolo"
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

type ReviewDecision string

const (
	ReviewAllow ReviewDecision = "allow"
	ReviewDeny  ReviewDecision = "deny"
)

type Check struct {
	Tier    Tier
	Preview string
}

type ReviewRequest struct {
	ToolCallID string
	ToolName   string
	Preview    string
}

type Reviewer func(context.Context, ReviewRequest) ReviewDecision

var levelRank = map[string]int{
	"readonly": 0,
	"safe":     1,
	"standard": 2,
	"yolo":     3,
}

var shellControlTokens = map[string]struct{}{
	"&&": {}, "||": {}, ";": {}, "|": {}, "|&": {}, "&": {},
	">": {}, ">>": {}, ">|": {}, "&>": {}, ">&": {},
	"<": {}, "<<": {}, "<<<": {}, "<&": {},
}

var dangerousPrograms = map[string]struct{}{
	"rm": {}, "rmdir": {}, "mv": {}, "cp": {}, "sudo": {}, "chmod": {}, "chown": {},
	"kill": {}, "pkill": {}, "dd": {}, "mkfs": {}, "mount": {}, "umount": {},
	"shutdown": {}, "reboot": {},
}

var dangerousGitSubcommands = map[string]struct{}{
	"reset": {}, "clean": {}, "checkout": {}, "restore": {},
}

var readonlyPrograms = map[string]struct{}{
	"pwd": {}, "ls": {}, "dir": {}, "tree": {}, "rg": {}, "grep": {}, "cat": {},
	"head": {}, "tail": {}, "wc": {}, "stat": {}, "file": {}, "du": {}, "df": {},
	"which": {}, "env": {}, "printenv": {}, "date": {}, "uname": {}, "whoami": {},
	"id": {}, "hostname": {}, "ps": {}, "uptime": {}, "realpath": {}, "dirname": {},
	"basename": {}, "sort": {}, "uniq": {}, "cut": {}, "tr": {},
}

var readonlyGitSubcommands = map[string]struct{}{
	"status": {}, "diff": {}, "log": {}, "show": {}, "rev-parse": {}, "ls-files": {},
	"grep": {}, "blame": {}, "describe": {},
}

var readonlyBranchFlags = map[string]struct{}{
	"-a": {}, "-r": {}, "-v": {}, "-vv": {}, "--all": {}, "--remotes": {},
	"--verbose": {}, "--show-current": {},
}

var findDangerousFlags = map[string]struct{}{
	"-delete": {}, "-exec": {}, "-execdir": {}, "-ok": {}, "-okdir": {},
	"-fprint": {}, "-fprint0": {}, "-fprintf": {}, "-fls": {},
}

func DecisionFor(permission config.PermissionConfig, tier Tier) Decision {
	if permission.Level == "" {
		permission = config.DefaultPermissionConfig()
	}
	if permission.Level == "yolo" {
		return DecisionAllow
	}
	if tier != TierYolo && levelRank[string(tier)] <= levelRank[permission.Level] {
		return DecisionAllow
	}
	if permission.Mode == "deny" {
		return DecisionDeny
	}
	return DecisionAsk
}

func ClassifyTool(toolName string, input map[string]any, cwd string, skillRoots []string) Check {
	name := strings.ToLower(strings.TrimSpace(toolName))
	switch name {
	case "bash":
		command := strings.TrimSpace(asString(input["command"]))
		return Check{Tier: classifyBash(command), Preview: command}
	case "read", "write", "edit":
		raw := asString(input["path"])
		path := tools.ResolvePath(raw, cwd)
		absPath, err := filepath.Abs(path)
		if err == nil {
			path = filepath.Clean(absPath)
		}
		preview := raw
		if strings.TrimSpace(preview) == "" {
			preview = path
		}
		if name == "read" && isUnderAny(path, skillRoots) {
			return Check{Tier: TierReadonly, Preview: preview}
		}
		if !isUnder(path, cwd) {
			return Check{Tier: TierYolo, Preview: preview}
		}
		if name == "read" {
			return Check{Tier: TierReadonly, Preview: preview}
		}
		return Check{Tier: TierSafe, Preview: preview}
	default:
		return Check{Tier: TierYolo, Preview: toolName}
	}
}

func SkillRoots(cwd, home string) []string {
	skills := prompt.DiscoverSkills(cwd, home)
	roots := make([]string, 0, len(skills))
	seen := map[string]struct{}{}
	for _, skill := range skills {
		root := filepath.Dir(skill.Path)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	return roots
}

func classifyBash(command string) Tier {
	if strings.TrimSpace(command) == "" {
		return TierYolo
	}
	if strings.ContainsAny(command, "\n\r") || strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return TierYolo
	}

	tokens, err := shellTokens(command)
	if err != nil || len(tokens) == 0 {
		return TierYolo
	}
	for _, token := range tokens {
		if _, ok := shellControlTokens[token]; ok || isPunctuationToken(token) {
			return TierYolo
		}
	}

	words := nonPunctuationTokens(tokens)
	if len(words) == 0 {
		return TierYolo
	}

	program := filepath.Base(words[0])
	if isDangerous(program, words) {
		return TierYolo
	}
	if isReadonly(program, words) {
		return TierReadonly
	}
	return TierStandard
}

func shellTokens(command string) ([]string, error) {
	tokens := []string{}
	var current strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for _, ch := range command {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				current.WriteRune(ch)
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		if ch == ' ' || ch == '\t' {
			flush()
			continue
		}
		if isShellPunctuation(ch) {
			flush()
			var token strings.Builder
			token.WriteRune(ch)
			tokens = append(tokens, token.String())
			continue
		}
		current.WriteRune(ch)
	}
	if escaped || quote != 0 {
		return nil, errInvalidShellCommand{}
	}
	flush()

	return mergePunctuation(tokens), nil
}

func mergePunctuation(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if len(token) == 1 && isShellPunctuation(rune(token[0])) && len(out) > 0 {
			prev := out[len(out)-1]
			if len(prev) == 1 && isShellPunctuation(rune(prev[0])) {
				out[len(out)-1] = prev + token
				continue
			}
		}
		out = append(out, token)
	}
	return out
}

func nonPunctuationTokens(tokens []string) []string {
	words := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if _, ok := shellControlTokens[token]; ok || isPunctuationToken(token) {
			continue
		}
		words = append(words, token)
	}
	return words
}

func isShellPunctuation(ch rune) bool {
	return ch == '&' || ch == '|' || ch == ';' || ch == '>' || ch == '<'
}

func isPunctuationToken(token string) bool {
	for _, ch := range token {
		if !isShellPunctuation(ch) {
			return false
		}
	}
	return token != ""
}

func isDangerous(program string, words []string) bool {
	if _, ok := dangerousPrograms[program]; ok {
		return true
	}
	if program == "sed" {
		return slices.ContainsFunc(words[1:], func(word string) bool {
			return word == "-i" || strings.HasPrefix(word, "-i")
		})
	}
	if program == "find" {
		return slices.ContainsFunc(words[1:], func(word string) bool {
			_, ok := findDangerousFlags[word]
			return ok
		})
	}
	if program != "git" {
		return false
	}
	sub := gitSubcommand(words)
	if _, ok := dangerousGitSubcommands[sub]; ok {
		return true
	}
	if sub != "push" {
		return false
	}
	return slices.ContainsFunc(words[2:], func(word string) bool {
		return word == "-f" || word == "--force" || word == "--force-with-lease"
	})
}

func isReadonly(program string, words []string) bool {
	if len(words) == 2 && (words[1] == "--version" || words[1] == "-v" || words[1] == "version") {
		return true
	}
	if _, ok := readonlyPrograms[program]; ok {
		return true
	}
	if program == "find" {
		return true
	}
	if program == "command" && len(words) >= 3 && words[1] == "-v" {
		return true
	}
	if program == "type" && len(words) >= 2 {
		return true
	}
	if program != "git" {
		return false
	}
	sub := gitSubcommand(words)
	if _, ok := readonlyGitSubcommands[sub]; ok {
		return true
	}
	if sub == "remote" {
		return !slices.ContainsFunc(words[2:], func(word string) bool {
			return word == "add" || word == "remove" || word == "rm" || word == "rename" || word == "set-url"
		})
	}
	if sub == "branch" {
		return allInSet(words[2:], readonlyBranchFlags)
	}
	return false
}

func gitSubcommand(words []string) string {
	for i := 1; i < len(words); {
		word := words[i]
		if word == "-C" || word == "-c" || word == "--git-dir" || word == "--work-tree" {
			i += 2
			continue
		}
		if strings.HasPrefix(word, "-") {
			i++
			continue
		}
		return word
	}
	return ""
}

func allInSet(words []string, set map[string]struct{}) bool {
	for _, word := range words {
		if _, ok := set[word]; !ok {
			return false
		}
	}
	return true
}

func isUnderAny(path string, roots []string) bool {
	for _, root := range roots {
		if isUnder(path, root) {
			return true
		}
	}
	return false
}

func isUnder(path, root string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = filepath.Clean(path)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = filepath.Clean(root)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}

type errInvalidShellCommand struct{}

func (errInvalidShellCommand) Error() string {
	return "invalid shell command"
}
