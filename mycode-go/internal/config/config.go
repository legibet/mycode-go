package config

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/legibet/mycode-go/internal/models"
	"github.com/legibet/mycode-go/internal/provider"
)

const (
	defaultHome             = "~/.mycode"
	defaultPort             = 8000
	defaultContextWindow    = 128000
	defaultMaxOutputTokens  = 16384
	defaultCompactThreshold = 0.8
)

var validReasoningEfforts = []string{"none", "low", "medium", "high", "xhigh"}
var validPermissionLevels = []string{"readonly", "safe", "standard", "yolo"}
var validPermissionModes = []string{"ask", "deny"}

var lookupModelMetadata = models.Lookup

var ErrUnsupportedReasoningEffort = errors.New("unsupported reasoning_effort")

// ModelConfig overrides bundled metadata for one model.
type ModelConfig struct {
	ContextWindow      int   `json:"context_window"`
	MaxOutputTokens    int   `json:"max_output_tokens"`
	SupportsReasoning  *bool `json:"supports_reasoning,omitempty"`
	SupportsImageInput *bool `json:"supports_image_input,omitempty"`
	SupportsPDFInput   *bool `json:"supports_pdf_input,omitempty"`
}

// ProviderConfig defines one configured provider alias.
type ProviderConfig struct {
	Name            string                 `json:"-"`
	Type            string                 `json:"type"`
	Models          map[string]ModelConfig `json:"models"`
	ModelOrder      []string               `json:"-"`
	APIKey          string                 `json:"-"`
	APIKeyEnvVar    string                 `json:"-"`
	BaseURL         string                 `json:"base_url"`
	ReasoningEffort string                 `json:"reasoning_effort"`
}

// PermissionConfig controls automatic tool execution.
type PermissionConfig struct {
	Level string `json:"level"`
	Mode  string `json:"mode"`
}

// Settings is the resolved config view for one cwd.
type Settings struct {
	Providers              map[string]ProviderConfig
	DefaultProvider        string
	DefaultModel           string
	DefaultReasoningEffort string
	CompactThreshold       float64
	Permission             PermissionConfig
	Port                   int
	CWD                    string
	Project                string
	ConfigPaths            []string

	providerOrder []string
}

// ResolvedProvider is the runnable provider config.
type ResolvedProvider struct {
	ProviderName         string
	ProviderType         string
	Model                string
	APIKey               string
	APIBase              string
	ReasoningEffort      string
	MaxTokens            int
	ContextWindow        int
	SupportsReasoning    bool
	SupportsImageInput   bool
	SupportsPDFInput     bool
	SupportsEffortToggle bool
}

type loadedConfig struct {
	Raw           map[string]any
	ProviderOrder []string
	ModelOrder    map[string][]string
}

type orderedObject struct {
	order  []string
	values map[string]json.RawMessage
}

// ResolveHome returns the mycode home directory.
func ResolveHome() string {
	raw := strings.TrimSpace(os.Getenv("MYCODE_HOME"))
	if raw == "" {
		raw = defaultHome
	}
	return absPath(raw)
}

// ResolveSessionsDir returns the persisted sessions directory.
func ResolveSessionsDir() string {
	return filepath.Join(ResolveHome(), "sessions")
}

// ResolveProject returns the nearest Git project root, or cwd when no .git is found.
func ResolveProject(cwd string) string {
	cwdPath := absPath(cwd)
	for path := cwdPath; path != "" && path != "." && path != "/"; path = filepath.Dir(path) {
		if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && info.IsDir() {
			return path
		}
		if path == filepath.Dir(path) {
			break
		}
	}
	return cwdPath
}

// ProjectDirs returns directories from project to cwd, inclusive.
func ProjectDirs(cwd, project string) []string {
	cwdPath := absPath(cwd)
	projectPath := absPath(project)

	dirs := []string{cwdPath}
	for dirs[len(dirs)-1] != projectPath {
		parent := filepath.Dir(dirs[len(dirs)-1])
		if parent == dirs[len(dirs)-1] {
			break
		}
		dirs = append(dirs, parent)
	}

	// Reverse so project is first, cwd is last.
	reversed := make([]string, len(dirs))
	for i, d := range dirs {
		reversed[len(dirs)-1-i] = d
	}
	return reversed
}

// Load returns merged config for one cwd.
func Load(cwd string) (Settings, error) {
	resolvedCWD := absPath(cmp.Or(cwd, mustGetwd()))
	resolvedProject := ResolveProject(resolvedCWD)

	settings := Settings{
		Providers:        map[string]ProviderConfig{},
		CompactThreshold: defaultCompactThreshold,
		Permission:       DefaultPermissionConfig(),
		Port:             defaultPort,
		CWD:              resolvedCWD,
		Project:          resolvedProject,
	}

	mergedProviders := map[string]map[string]any{}
	mergedModelOrder := map[string][]string{}
	providerOrder := []string{}
	seenProviders := map[string]struct{}{}

	configPaths := []string{filepath.Join(ResolveHome(), "config.json")}
	for _, dir := range ProjectDirs(resolvedCWD, resolvedProject) {
		configPaths = append(configPaths, filepath.Join(dir, ".mycode", "config.json"))
	}
	for _, path := range configPaths {
		loaded, ok := loadConfig(path)
		if !ok {
			continue
		}
		raw := loaded.Raw
		settings.ConfigPaths = append(settings.ConfigPaths, path)

		if rawDefault, ok := raw["default"].(map[string]any); ok {
			if value, ok := rawDefault["provider"].(string); ok {
				settings.DefaultProvider = strings.TrimSpace(value)
			}
			if value, ok := rawDefault["model"].(string); ok {
				settings.DefaultModel = strings.TrimSpace(value)
			}
			if _, exists := rawDefault["reasoning_effort"]; exists {
				settings.DefaultReasoningEffort = normalizeReasoningEffort(rawDefault["reasoning_effort"])
			}
			if threshold, ok := parseCompactThreshold(rawDefault["compact_threshold"]); ok {
				settings.CompactThreshold = threshold
			}
		}

		if rawPermission, exists := raw["permission"]; exists {
			permission, err := parsePermissionConfig(rawPermission, settings.Permission)
			if err != nil {
				return Settings{}, err
			}
			settings.Permission = permission
		}

		rawProviders, _ := raw["providers"].(map[string]any)
		keys := loaded.ProviderOrder
		if len(keys) == 0 {
			for name := range rawProviders {
				keys = append(keys, name)
			}
			slices.Sort(keys)
		}
		for _, name := range keys {
			entry, ok := rawProviders[name].(map[string]any)
			if !ok {
				continue
			}
			if _, seen := seenProviders[name]; !seen {
				seenProviders[name] = struct{}{}
				providerOrder = append(providerOrder, name)
			}
			merged := maps.Clone(mergedProviders[name])
			if merged == nil {
				merged = map[string]any{}
			}
			if _, exists := entry["type"]; exists {
				merged["type"] = entry["type"]
			}
			if _, exists := entry["models"]; exists {
				merged["models"] = entry["models"]
				if order := loaded.ModelOrder[name]; len(order) > 0 {
					mergedModelOrder[name] = append([]string(nil), order...)
				} else {
					delete(mergedModelOrder, name)
				}
			}
			if _, exists := entry["api_key"]; exists {
				apiKey, apiKeyEnvVar := parseConfigAPIKey(entry["api_key"])
				merged["api_key"] = apiKey
				merged["api_key_env_var"] = apiKeyEnvVar
			}
			if _, exists := entry["base_url"]; exists {
				merged["base_url"] = entry["base_url"]
			}
			if _, exists := entry["reasoning_effort"]; exists {
				merged["reasoning_effort"] = entry["reasoning_effort"]
			}
			mergedProviders[name] = merged
		}
	}

	providers, err := buildProviders(mergedProviders, providerOrder, mergedModelOrder)
	if err != nil {
		return Settings{}, err
	}
	settings.Providers = providers
	settings.providerOrder = providerOrder

	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		parsed, err := strconv.Atoi(port)
		if err == nil && parsed > 0 {
			settings.Port = parsed
		}
	}

	return settings, nil
}

func DefaultPermissionConfig() PermissionConfig {
	return PermissionConfig{Level: "safe", Mode: "ask"}
}

// ResolveProvider resolves one provider alias or built-in provider id.
func ResolveProvider(settings Settings, providerName, model, apiKey, apiBase, reasoningEffort string) (ResolvedProvider, error) {
	selected := strings.TrimSpace(providerName)
	if selected == "" {
		selected = strings.TrimSpace(settings.DefaultProvider)
	}
	if selected == "" {
		available := availableProviderReferences(settings)
		if len(available) > 0 {
			selected = available[0]
		}
	}
	if selected != "" {
		return resolveProviderRuntime(settings, selected, model, apiKey, apiBase, reasoningEffort)
	}

	envNames := []string{}
	seen := map[string]struct{}{}
	for _, spec := range provider.Specs() {
		if !spec.AutoDiscoverable {
			continue
		}
		for _, envName := range spec.EnvAPIKeyNames {
			if _, ok := seen[envName]; ok {
				continue
			}
			seen[envName] = struct{}{}
			envNames = append(envNames, envName)
		}
	}
	checked := "<api key env>"
	if len(envNames) > 0 {
		checked = strings.Join(envNames, ", ")
	}
	return ResolvedProvider{}, fmt.Errorf("no available providers found; set one of the supported API key env vars (%s) or configure a provider in ~/.mycode/config.json or a project .mycode/config.json", checked)
}

// AvailableProviders returns currently selectable providers in stable order.
func AvailableProviders(settings Settings) []ResolvedProvider {
	names := availableProviderReferences(settings)
	out := make([]ResolvedProvider, 0, len(names))
	for _, name := range names {
		resolved, err := resolveProviderRuntime(settings, name, "", "", "", "")
		if err == nil {
			out = append(out, resolved)
		}
	}
	return out
}

func availableProviderReferences(settings Settings) []string {
	names := []string{}
	seen := map[string]struct{}{}
	configuredTypesWithCredentials := map[string]struct{}{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		configured, hasConfig := settings.Providers[name]
		providerType := name
		if hasConfig {
			providerType = configured.Type
		}
		if _, ok := provider.LookupSpec(providerType); !ok {
			return
		}
		if hasConfig {
			if !providerHasAPIKey(configured) {
				return
			}
			configuredTypesWithCredentials[providerType] = struct{}{}
		} else if apiKeyFromEnv(providerType) == "" {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	add(settings.DefaultProvider)
	for _, name := range settings.providerOrder {
		if providerHasAPIKey(settings.Providers[name]) {
			add(name)
		}
	}
	for _, spec := range provider.Specs() {
		if !spec.AutoDiscoverable {
			continue
		}
		if _, skip := configuredTypesWithCredentials[spec.ID]; skip {
			continue
		}
		if apiKeyFromEnv(spec.ID) == "" {
			continue
		}
		add(spec.ID)
	}
	return names
}

func resolveProviderRuntime(settings Settings, selectedName, model, apiKey, apiBase, reasoningEffort string) (ResolvedProvider, error) {
	configured, hasConfig := settings.Providers[selectedName]
	providerType := cmp.Or(configured.Type, selectedName)
	spec, ok := provider.LookupSpec(providerType)
	if !ok {
		supported := []string{}
		for _, candidate := range provider.Specs() {
			supported = append(supported, candidate.ID)
		}
		slices.Sort(supported)
		return ResolvedProvider{}, fmt.Errorf("unsupported provider %q; supported: %s", providerType, strings.Join(supported, ", "))
	}

	resolvedModel := strings.TrimSpace(model)
	switch {
	case resolvedModel != "":
		// already set by request
	case hasConfig && len(configured.Models) > 0 && len(configured.ModelOrder) > 0:
		resolvedModel = configured.ModelOrder[0]
	case hasConfig && len(configured.Models) > 0:
		resolvedModel = slices.Sorted(maps.Keys(configured.Models))[0]
	case selectedName == settings.DefaultProvider && strings.TrimSpace(settings.DefaultModel) != "":
		resolvedModel = strings.TrimSpace(settings.DefaultModel)
	case len(spec.DefaultModels) > 0:
		resolvedModel = spec.DefaultModels[0]
	default:
		return ResolvedProvider{}, fmt.Errorf("provider %q does not define any default models", selectedName)
	}

	meta := resolveMetadata(providerType, resolvedModel, hasConfig, configured)
	supportsReasoning := meta != nil && meta.SupportsReasoning != nil && *meta.SupportsReasoning
	supportsImageInput := meta != nil && meta.SupportsImageInput != nil && *meta.SupportsImageInput
	supportsPDFInput := meta != nil && meta.SupportsPDFInput != nil && *meta.SupportsPDFInput

	// Request values win, then provider config, then global defaults.
	configuredEffort := normalizeReasoningEffort(reasoningEffort)
	switch {
	case configuredEffort != "":
		// from request
	case hasConfig && configured.ReasoningEffort != "":
		configuredEffort = normalizeReasoningEffort(configured.ReasoningEffort)
	case settings.DefaultReasoningEffort != "":
		configuredEffort = normalizeReasoningEffort(settings.DefaultReasoningEffort)
	}
	if configuredEffort != "" {
		if !slices.Contains(validReasoningEfforts, configuredEffort) {
			return ResolvedProvider{}, fmt.Errorf("%w %q; supported: %s",
				ErrUnsupportedReasoningEffort, configuredEffort, strings.Join(validReasoningEfforts, ", "))
		}
		if !supportsReasoning || !spec.SupportsReasoningEffort {
			configuredEffort = ""
		}
	}

	resolvedAPIBase := cmp.Or(
		strings.TrimSpace(apiBase),
		strings.TrimSpace(configured.BaseURL),
		spec.DefaultBaseURL,
	)

	resolvedAPIKey := strings.TrimSpace(apiKey)
	switch {
	case resolvedAPIKey != "":
		// from request
	case hasConfig && configured.APIKeyEnvVar != "":
		resolvedAPIKey = strings.TrimSpace(os.Getenv(configured.APIKeyEnvVar))
		if resolvedAPIKey == "" {
			return ResolvedProvider{}, fmt.Errorf("missing API key env var %q referenced by provider %q", configured.APIKeyEnvVar, selectedName)
		}
	case hasConfig && configured.APIKey != "":
		resolvedAPIKey = configured.APIKey
	}
	if resolvedAPIKey == "" {
		resolvedAPIKey = apiKeyFromEnv(providerType)
	}
	if resolvedAPIKey == "" {
		checked := cmp.Or(strings.Join(spec.EnvAPIKeyNames, ", "), "<api key env>")
		return ResolvedProvider{}, fmt.Errorf("provider %q is selected but no API key is available; checked: %s", selectedName, checked)
	}

	maxTokens, contextWindow := defaultMaxOutputTokens, defaultContextWindow
	if meta != nil {
		maxTokens = defaultInt(meta.MaxOutputTokens, defaultMaxOutputTokens)
		contextWindow = defaultInt(meta.ContextWindow, defaultContextWindow)
	}
	return ResolvedProvider{
		ProviderName:         selectedName,
		ProviderType:         providerType,
		Model:                resolvedModel,
		APIKey:               resolvedAPIKey,
		APIBase:              resolvedAPIBase,
		ReasoningEffort:      configuredEffort,
		MaxTokens:            maxTokens,
		ContextWindow:        contextWindow,
		SupportsReasoning:    supportsReasoning,
		SupportsImageInput:   supportsImageInput,
		SupportsPDFInput:     supportsPDFInput,
		SupportsEffortToggle: spec.SupportsReasoningEffort,
	}, nil
}

func resolveMetadata(providerType, model string, hasConfig bool, configured ProviderConfig) *models.Metadata {
	meta := lookupModelMetadata(providerType, model)
	if !hasConfig {
		return meta
	}
	override, ok := configured.Models[model]
	if !ok {
		return meta
	}
	if meta == nil {
		meta = &models.Metadata{Provider: providerType, Model: model}
	}
	if override.ContextWindow > 0 {
		meta.ContextWindow = override.ContextWindow
	}
	if override.MaxOutputTokens > 0 {
		meta.MaxOutputTokens = override.MaxOutputTokens
	}
	if override.SupportsReasoning != nil {
		meta.SupportsReasoning = override.SupportsReasoning
	}
	if override.SupportsImageInput != nil {
		meta.SupportsImageInput = override.SupportsImageInput
	}
	if override.SupportsPDFInput != nil {
		meta.SupportsPDFInput = override.SupportsPDFInput
	}
	return meta
}

func providerHasAPIKey(providerConfig ProviderConfig) bool {
	if providerConfig.APIKeyEnvVar != "" {
		return strings.TrimSpace(os.Getenv(providerConfig.APIKeyEnvVar)) != ""
	}
	if providerConfig.APIKey != "" {
		return true
	}
	return apiKeyFromEnv(providerConfig.Type) != ""
}

func buildProviders(rawProviders map[string]map[string]any, order []string, modelOrder map[string][]string) (map[string]ProviderConfig, error) {
	providers := map[string]ProviderConfig{}
	for _, name := range order {
		raw := rawProviders[name]
		rawType, hasExplicitType := raw["type"]
		providerType := strings.TrimSpace(asString(rawType))
		if hasExplicitType && providerType == "" {
			providerType = "anthropic"
		}
		if providerType == "" {
			if _, ok := provider.LookupSpec(name); ok {
				providerType = name
			} else {
				return nil, fmt.Errorf("provider %q must set 'type'", name)
			}
		}
		if _, ok := provider.LookupSpec(providerType); !ok {
			return nil, fmt.Errorf("unsupported provider type %q", providerType)
		}

		modelsMap, orderedModels := normalizeModels(raw["models"], modelOrder[name])
		if len(modelsMap) == 0 {
			spec, _ := provider.LookupSpec(providerType)
			modelsMap = make(map[string]ModelConfig, len(spec.DefaultModels))
			for _, model := range spec.DefaultModels {
				modelsMap[model] = ModelConfig{}
			}
			orderedModels = append([]string(nil), spec.DefaultModels...)
		}

		providers[name] = ProviderConfig{
			Name:            name,
			Type:            providerType,
			Models:          modelsMap,
			ModelOrder:      orderedModels,
			APIKey:          strings.TrimSpace(asString(raw["api_key"])),
			APIKeyEnvVar:    strings.TrimSpace(asString(raw["api_key_env_var"])),
			BaseURL:         strings.TrimSpace(asString(raw["base_url"])),
			ReasoningEffort: normalizeReasoningEffort(raw["reasoning_effort"]),
		}
	}
	return providers, nil
}

func normalizeModels(raw any, order []string) (map[string]ModelConfig, []string) {
	modelMap, _ := raw.(map[string]any)
	if len(modelMap) == 0 {
		return nil, nil
	}
	out := make(map[string]ModelConfig, len(modelMap))
	keys := make([]string, 0, len(modelMap))
	seen := map[string]struct{}{}
	for _, name := range order {
		if _, ok := modelMap[name]; ok {
			keys = append(keys, name)
			seen[name] = struct{}{}
		}
	}
	extra := make([]string, 0, len(modelMap))
	for name := range modelMap {
		if _, ok := seen[name]; !ok {
			extra = append(extra, name)
		}
	}
	slices.Sort(extra)
	keys = append(keys, extra...)
	for _, name := range keys {
		rawConfig, _ := modelMap[name].(map[string]any)
		cfg := ModelConfig{}
		if rawConfig != nil {
			cfg.ContextWindow = asInt(rawConfig["context_window"])
			cfg.MaxOutputTokens = asInt(rawConfig["max_output_tokens"])
			cfg.SupportsReasoning = asBoolPtr(rawConfig["supports_reasoning"])
			cfg.SupportsImageInput = asBoolPtr(rawConfig["supports_image_input"])
			cfg.SupportsPDFInput = asBoolPtr(rawConfig["supports_pdf_input"])
		}
		out[name] = cfg
	}
	return out, keys
}

func parseCompactThreshold(value any) (float64, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case bool:
		if !v {
			return 0, true
		}
		return 0, false
	case float64:
		if v < 0 || v > 1 {
			return 0, false
		}
		return v, true
	case int:
		if v < 0 || v > 1 {
			return 0, false
		}
		return float64(v), true
	default:
		return 0, false
	}
}

func normalizeReasoningEffort(value any) string {
	text := strings.TrimSpace(strings.ToLower(asString(value)))
	switch text {
	case "", "auto", "default":
		return ""
	case "off", "disabled":
		return "none"
	default:
		return text
	}
}

func parsePermissionConfig(value any, current PermissionConfig) (PermissionConfig, error) {
	if current.Level == "" {
		current = DefaultPermissionConfig()
	}

	switch raw := value.(type) {
	case nil:
		return current, nil
	case string:
		level, err := normalizePermissionLevel(raw)
		if err != nil {
			return PermissionConfig{}, err
		}
		return PermissionConfig{Level: level, Mode: current.Mode}, nil
	case map[string]any:
		next := current
		if value, exists := raw["level"]; exists {
			level, err := normalizePermissionLevel(value)
			if err != nil {
				return PermissionConfig{}, err
			}
			next.Level = level
		}
		if value, exists := raw["mode"]; exists {
			mode, err := normalizePermissionMode(value)
			if err != nil {
				return PermissionConfig{}, err
			}
			next.Mode = mode
		}
		return next, nil
	default:
		return PermissionConfig{}, fmt.Errorf("permission must be a string or object, got %T", value)
	}
}

func normalizePermissionLevel(value any) (string, error) {
	level := strings.TrimSpace(strings.ToLower(asString(value)))
	if slices.Contains(validPermissionLevels, level) {
		return level, nil
	}
	return "", fmt.Errorf("unsupported permission level %q; supported: %s", asString(value), strings.Join(validPermissionLevels, ", "))
}

func normalizePermissionMode(value any) (string, error) {
	mode := strings.TrimSpace(strings.ToLower(asString(value)))
	if slices.Contains(validPermissionModes, mode) {
		return mode, nil
	}
	return "", fmt.Errorf("unsupported permission mode %q; supported: %s", asString(value), strings.Join(validPermissionModes, ", "))
}

func parseConfigAPIKey(value any) (literal string, envVar string) {
	text := strings.TrimSpace(asString(value))
	if text == "" {
		return "", ""
	}
	if strings.HasPrefix(text, "${") && strings.HasSuffix(text, "}") {
		return "", strings.TrimSuffix(strings.TrimPrefix(text, "${"), "}")
	}
	return text, ""
}

func apiKeyFromEnv(providerType string) string {
	spec, ok := provider.LookupSpec(providerType)
	if !ok {
		return ""
	}
	for _, name := range spec.EnvAPIKeyNames {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func loadConfig(path string) (loadedConfig, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return loadedConfig{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return loadedConfig{}, false
	}

	loaded := loadedConfig{Raw: raw, ModelOrder: map[string][]string{}}
	root, err := parseOrderedObject(data)
	if err != nil {
		return loaded, true
	}
	rawProviders, ok := root.values["providers"]
	if !ok {
		return loaded, true
	}
	providers, err := parseOrderedObject(rawProviders)
	if err != nil {
		return loaded, true
	}
	loaded.ProviderOrder = append([]string(nil), providers.order...)
	for _, name := range providers.order {
		rawProvider, ok := providers.values[name]
		if !ok {
			continue
		}
		providerObject, err := parseOrderedObject(rawProvider)
		if err != nil {
			continue
		}
		rawModels, ok := providerObject.values["models"]
		if !ok {
			continue
		}
		modelsObject, err := parseOrderedObject(rawModels)
		if err != nil {
			continue
		}
		loaded.ModelOrder[name] = append([]string(nil), modelsObject.order...)
	}
	return loaded, true
}

func parseOrderedObject(data []byte) (orderedObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return orderedObject{}, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return orderedObject{}, fmt.Errorf("expected object")
	}
	result := orderedObject{values: map[string]json.RawMessage{}}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return orderedObject{}, err
		}
		key, ok := token.(string)
		if !ok {
			return orderedObject{}, fmt.Errorf("expected object key")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return orderedObject{}, err
		}
		result.order = append(result.order, key)
		result.values[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return orderedObject{}, err
	}
	return result, nil
}

func defaultInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func absPath(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}

func asInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}

func asBoolPtr(value any) *bool {
	if v, ok := value.(bool); ok {
		return &v
	}
	return nil
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}
