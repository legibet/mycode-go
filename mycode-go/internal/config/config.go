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
	"github.com/legibet/mycode-go/internal/util"
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

type ConfigOrder struct {
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
	return util.ExpandAbs(raw)
}

// ResolveSessionsDir returns the persisted sessions directory.
func ResolveSessionsDir() string {
	return filepath.Join(ResolveHome(), "sessions")
}

// ResolveProject returns the nearest Git project root, or cwd when no .git is found.
func ResolveProject(cwd string) string {
	cwdPath := util.ResolveSymlinks(cwd)
	for path := cwdPath; path != "" && path != "."; {
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return cwdPath
}

// ProjectDirs returns directories from project to cwd, inclusive.
func ProjectDirs(cwd, project string) []string {
	cwdPath := util.ResolveSymlinks(cwd)
	projectPath := util.ResolveSymlinks(project)
	if projectPath == "" {
		projectPath = ResolveProject(cwdPath)
	}

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
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return Settings{}, err
		}
		cwd = wd
	}
	resolvedCWD := util.ResolveSymlinks(cwd)
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

	configPaths := []string{util.ResolveSymlinks(filepath.Join(ResolveHome(), "config.json"))}
	for _, dir := range ProjectDirs(resolvedCWD, resolvedProject) {
		configPaths = append(configPaths, util.ResolveSymlinks(filepath.Join(dir, ".mycode", "config.json")))
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
			if value, exists := rawDefault["reasoning_effort"]; exists {
				if text, ok := value.(string); ok {
					if err := validateReasoningEffort(text); err != nil {
						return Settings{}, err
					}
					settings.DefaultReasoningEffort = normalizeReasoningEffort(text)
				} else {
					settings.DefaultReasoningEffort = ""
				}
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
					mergedModelOrder[name] = slices.Clone(order)
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

func PermissionLevelOptions() []string {
	return slices.Clone(validPermissionLevels)
}

func PermissionModeOptions() []string {
	return slices.Clone(validPermissionModes)
}

// ParseConfigOrder preserves model order lost by map unmarshalling.
func ParseConfigOrder(data []byte) ConfigOrder {
	out := ConfigOrder{ModelOrder: map[string][]string{}}
	root, err := parseOrderedObject(data)
	if err != nil {
		return out
	}
	rawProviders, ok := root.values["providers"]
	if !ok {
		return out
	}
	providers, err := parseOrderedObject(rawProviders)
	if err != nil {
		return out
	}
	out.ProviderOrder = cleanStringOrder(providers.order)
	for _, name := range providers.order {
		providerObject, err := parseOrderedObject(providers.values[name])
		if err != nil {
			continue
		}
		rawModels := bytes.TrimSpace(providerObject.values["models"])
		if len(rawModels) == 0 {
			continue
		}
		if rawModels[0] == '[' {
			var models []any
			if err := json.Unmarshal(rawModels, &models); err != nil {
				continue
			}
			order := make([]string, 0, len(models))
			for _, model := range models {
				if text, ok := model.(string); ok {
					order = append(order, text)
				}
			}
			out.ModelOrder[strings.TrimSpace(name)] = cleanStringOrder(order)
			continue
		}
		models, err := parseOrderedObject(rawModels)
		if err != nil {
			continue
		}
		out.ModelOrder[strings.TrimSpace(name)] = cleanStringOrder(models.order)
	}
	return out
}

func NormalizeReasoningEffort(value string) (string, error) {
	if err := validateReasoningEffort(value); err != nil {
		return "", err
	}
	return normalizeReasoningEffort(value), nil
}

func IsAPIKeyEnvRef(value string) string {
	inner, ok := strings.CutPrefix(strings.TrimSpace(value), "${")
	if !ok {
		return ""
	}
	name, ok := strings.CutSuffix(inner, "}")
	if !ok || name == "" {
		return ""
	}
	for i, r := range name {
		letter := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if r == '_' || letter || i > 0 && digit {
			continue
		}
		return ""
	}
	return name
}

var modelOverrideKeys = map[string]struct{}{
	"context_window":       {},
	"max_output_tokens":    {},
	"supports_reasoning":   {},
	"supports_image_input": {},
	"supports_pdf_input":   {},
}

func ValidateGlobalConfig(data any) (map[string]any, error) {
	if data == nil {
		return map[string]any{}, nil
	}
	raw, ok := data.(map[string]any)
	if !ok {
		return nil, errors.New("config must be an object")
	}

	out := map[string]any{}
	if rawDefault, exists := raw["default"]; exists && rawDefault != nil {
		cleaned, err := validateDefaultConfig(rawDefault)
		if err != nil {
			return nil, err
		}
		if len(cleaned) > 0 {
			out["default"] = cleaned
		}
	}

	if rawPermission, exists := raw["permission"]; exists && rawPermission != nil {
		cleaned, err := validatePermissionPayload(rawPermission)
		if err != nil {
			return nil, err
		}
		out["permission"] = cleaned
	}

	if rawProviders, exists := raw["providers"]; exists && rawProviders != nil {
		providersRaw, ok := rawProviders.(map[string]any)
		if !ok {
			return nil, errors.New("providers must be an object")
		}
		providers := map[string]any{}
		for name, rawProvider := range providersRaw {
			cleanedName := strings.TrimSpace(name)
			if cleanedName == "" {
				return nil, errors.New("provider name must be a non-empty string")
			}
			cleaned, err := validateProviderPayload(cleanedName, rawProvider)
			if err != nil {
				return nil, err
			}
			providers[cleanedName] = cleaned
		}
		if len(providers) > 0 {
			out["providers"] = providers
		}
	}

	return out, nil
}

// ResolveProvider resolves one provider alias or built-in provider id.
func ResolveProvider(settings Settings, providerName, model, apiKey, apiBase string) (ResolvedProvider, error) {
	explicitName := strings.TrimSpace(providerName)
	if explicitName != "" {
		return resolveProviderRuntime(settings, explicitName, model, apiKey, apiBase)
	}

	defaultName := strings.TrimSpace(settings.DefaultProvider)
	if defaultName != "" {
		resolved, err := resolveProviderRuntime(settings, defaultName, model, apiKey, apiBase)
		if err == nil {
			return resolved, nil
		}
	}

	for _, name := range availableProviderReferences(settings) {
		if name == defaultName {
			continue
		}
		resolved, err := resolveProviderRuntime(settings, name, model, apiKey, apiBase)
		if err == nil {
			return resolved, nil
		}
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
		resolved, err := resolveProviderRuntime(settings, name, "", "", "")
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

func resolveProviderRuntime(settings Settings, selectedName, model, apiKey, apiBase string) (ResolvedProvider, error) {
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

	reasoningEffort := ""
	if supportsReasoning && spec.SupportsReasoningEffort {
		switch {
		case hasConfig && configured.ReasoningEffort != "":
			reasoningEffort = configured.ReasoningEffort
		case settings.DefaultReasoningEffort != "":
			reasoningEffort = settings.DefaultReasoningEffort
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
		ReasoningEffort:      reasoningEffort,
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
		providerType := ""
		if disabled, ok := rawType.(bool); ok && !disabled {
			providerType = ""
		} else if rawType != nil {
			providerType = fmt.Sprint(rawType)
		}
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
			orderedModels = slices.Clone(spec.DefaultModels)
		}
		reasoningEffort := ""
		if value, exists := raw["reasoning_effort"]; exists && value != nil {
			switch v := value.(type) {
			case string:
				if err := validateReasoningEffort(v); err != nil {
					return nil, err
				}
				reasoningEffort = normalizeReasoningEffort(v)
			case bool:
				if v {
					return nil, fmt.Errorf("reasoning_effort must be a string, got %T", value)
				}
			default:
				return nil, fmt.Errorf("reasoning_effort must be a string, got %T", value)
			}
		}

		providers[name] = ProviderConfig{
			Name:            name,
			Type:            providerType,
			Models:          modelsMap,
			ModelOrder:      orderedModels,
			APIKey:          strings.TrimSpace(asString(raw["api_key"])),
			APIKeyEnvVar:    strings.TrimSpace(asString(raw["api_key_env_var"])),
			BaseURL:         strings.TrimSpace(asString(raw["base_url"])),
			ReasoningEffort: reasoningEffort,
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

func validateDefaultConfig(value any) (map[string]any, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("default must be an object")
	}

	out := map[string]any{}
	for _, key := range []string{"provider", "model"} {
		value, err := optionalString(raw, key, "default."+key)
		if err != nil {
			return nil, err
		}
		if value != "" {
			out[key] = value
		}
	}

	if effort, exists := raw["reasoning_effort"]; exists && effort != nil && effort != "" {
		if err := validateReasoningEffort(effort); err != nil {
			return nil, err
		}
		out["reasoning_effort"] = asString(effort)
	}

	if threshold, exists := raw["compact_threshold"]; exists && threshold != nil {
		if disabled, ok := threshold.(bool); ok && !disabled {
			out["compact_threshold"] = false
			return out, nil
		}
		value, ok := parseCompactThreshold(threshold)
		if !ok {
			return nil, errors.New("default.compact_threshold must be a number in [0, 1] or false")
		}
		out["compact_threshold"] = value
	}

	return out, nil
}

func validatePermissionPayload(value any) (any, error) {
	if text, ok := value.(string); ok {
		return normalizePermissionLevel(text)
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("permission must be a string or object")
	}
	out := map[string]any{}
	if value, exists := raw["level"]; exists {
		level, err := normalizePermissionLevel(value)
		if err != nil {
			return nil, err
		}
		out["level"] = level
	}
	if value, exists := raw["mode"]; exists {
		mode, err := normalizePermissionMode(value)
		if err != nil {
			return nil, err
		}
		out["mode"] = mode
	}
	return out, nil
}

func validateProviderPayload(name string, value any) (map[string]any, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("provider %q must be an object", name)
	}
	out := map[string]any{}

	if rawType, exists := raw["type"]; exists && rawType != nil && rawType != "" {
		providerType, ok := rawType.(string)
		if !ok {
			return nil, fmt.Errorf("provider %q: type must be a string", name)
		}
		if _, ok := provider.LookupSpec(providerType); !ok {
			return nil, fmt.Errorf("provider %q: unsupported type %q; supported: %s", name, providerType, strings.Join(supportedProviderTypes(), ", "))
		}
		out["type"] = providerType
	} else if _, ok := provider.LookupSpec(name); !ok {
		return nil, fmt.Errorf("provider %q must set 'type'", name)
	}

	for _, key := range []string{"api_key", "base_url"} {
		value, err := optionalString(raw, key, fmt.Sprintf("provider %q: %s", name, key))
		if err != nil {
			return nil, err
		}
		if value != "" {
			out[key] = value
		}
	}

	if effort, exists := raw["reasoning_effort"]; exists && effort != nil && effort != "" {
		if err := validateReasoningEffort(effort); err != nil {
			return nil, err
		}
		out["reasoning_effort"] = asString(effort)
	}

	if rawModels, exists := raw["models"]; exists && rawModels != nil {
		models, err := validateModelsPayload(name, rawModels)
		if err != nil {
			return nil, err
		}
		if len(models) > 0 {
			out["models"] = models
		}
	}

	return out, nil
}

func validateModelsPayload(name string, value any) (map[string]any, error) {
	switch raw := value.(type) {
	case []any:
		models := map[string]any{}
		seen := map[string]struct{}{}
		for _, item := range raw {
			modelID, ok := item.(string)
			if !ok || strings.TrimSpace(modelID) == "" {
				return nil, fmt.Errorf("provider %q: model id must be a non-empty string", name)
			}
			cleaned := strings.TrimSpace(modelID)
			if _, ok := seen[cleaned]; ok {
				continue
			}
			seen[cleaned] = struct{}{}
			models[cleaned] = map[string]any{}
		}
		return models, nil
	case map[string]any:
		items := map[string]any{}
		order := []string{}
		for modelID, overrides := range raw {
			if strings.TrimSpace(modelID) == "" {
				return nil, fmt.Errorf("provider %q: model id must be a non-empty string", name)
			}
			cleaned := strings.TrimSpace(modelID)
			if overrides == nil {
				items[cleaned] = map[string]any{}
			} else if rawOverrides, ok := overrides.(map[string]any); ok {
				modelOverrides := map[string]any{}
				for key, overrideValue := range rawOverrides {
					if _, ok := modelOverrideKeys[key]; ok && overrideValue != nil {
						modelOverrides[key] = overrideValue
					}
				}
				items[cleaned] = modelOverrides
			} else {
				return nil, fmt.Errorf("provider %q: model %q config must be an object", name, cleaned)
			}
			order = append(order, cleaned)
		}
		if len(order) > 0 {
			slices.Sort(order)
		}
		out := map[string]any{}
		for _, modelID := range order {
			out[modelID] = items[modelID]
		}
		return out, nil
	default:
		return nil, fmt.Errorf("provider %q: models must be a list or object", name)
	}
}

func optionalString(raw map[string]any, key, label string) (string, error) {
	value, exists := raw[key]
	if !exists || value == nil || value == "" {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", label)
	}
	return strings.TrimSpace(text), nil
}

func validateReasoningEffort(value any) error {
	if _, ok := value.(string); !ok {
		return fmt.Errorf("reasoning_effort must be a string, got %T", value)
	}
	effort := normalizeReasoningEffort(value)
	if effort == "" || slices.Contains(validReasoningEfforts, effort) {
		return nil
	}
	return fmt.Errorf("%w %q; supported: auto, %s", ErrUnsupportedReasoningEffort, asString(value), strings.Join(validReasoningEfforts, ", "))
}

func supportedProviderTypes() []string {
	out := make([]string, 0, len(provider.Specs()))
	for _, spec := range provider.Specs() {
		out = append(out, spec.ID)
	}
	slices.Sort(out)
	return out
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
	case json.Number:
		parsed, err := v.Float64()
		if err != nil || parsed < 0 || parsed > 1 {
			return 0, false
		}
		return parsed, true
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
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("permission level must be a string, got %T", value)
	}
	level := strings.TrimSpace(strings.ToLower(text))
	if slices.Contains(validPermissionLevels, level) {
		return level, nil
	}
	return "", fmt.Errorf("unsupported permission level %q; supported: %s", asString(value), strings.Join(validPermissionLevels, ", "))
}

func normalizePermissionMode(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("permission mode must be a string, got %T", value)
	}
	mode := strings.TrimSpace(strings.ToLower(text))
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
	if ref := IsAPIKeyEnvRef(text); ref != "" {
		return "", ref
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

	order := ParseConfigOrder(data)
	return loadedConfig{
		Raw:           raw,
		ProviderOrder: order.ProviderOrder,
		ModelOrder:    order.ModelOrder,
	}, true
}

func parseOrderedObject(data []byte) (orderedObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return orderedObject{}, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return orderedObject{}, errors.New("expected object")
	}
	result := orderedObject{values: map[string]json.RawMessage{}}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return orderedObject{}, err
		}
		key, ok := token.(string)
		if !ok {
			return orderedObject{}, errors.New("expected object key")
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

func cleanStringOrder(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func defaultInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
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

// ----- Settings IO (server side: load, render, save the global config.json) -----

// ReasoningEffortOptions enumerates the values exposed to the web UI; "auto"
// is added on top of the validated set so the UI can render the unset case.
var ReasoningEffortOptions = []string{"auto", "none", "low", "medium", "high", "xhigh"}

func ResponseReasoningEffort(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// ReadRawSettings loads the global settings JSON. Returns an empty map and
// exists=false when the file is missing or not a regular file (e.g. a
// directory placeholder). Parse failures bubble up with the file path.
func ReadRawSettings(path string) (raw map[string]any, order ConfigOrder, exists bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, ConfigOrder{}, false, nil
	}
	if err != nil {
		return nil, ConfigOrder{}, false, err
	}
	if !info.Mode().IsRegular() {
		return map[string]any{}, ConfigOrder{}, false, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ConfigOrder{}, false, err
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, ConfigOrder{}, true, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	root, ok := parsed.(map[string]any)
	if !ok {
		return nil, ConfigOrder{}, true, fmt.Errorf("%s must contain a JSON object", path)
	}
	return root, ParseConfigOrder(data), true, nil
}

// BuildSettingsResponse renders the GET /api/settings payload. The web UI
// needs the saved config plus metadata about valid options / env vars to
// render the settings form without making extra calls.
func BuildSettingsResponse(path string, exists bool, raw map[string]any, order ConfigOrder) map[string]any {
	providerTypes := make([]string, 0, len(provider.Specs()))
	envNames := map[string]struct{}{}
	typeEnvVars := map[string][]string{}
	typeDefaultModels := map[string][]string{}
	for _, spec := range provider.Specs() {
		providerTypes = append(providerTypes, spec.ID)
		if len(spec.EnvAPIKeyNames) > 0 {
			typeEnvVars[spec.ID] = slices.Clone(spec.EnvAPIKeyNames)
		}
		if len(spec.DefaultModels) > 0 {
			typeDefaultModels[spec.ID] = slices.Clone(spec.DefaultModels)
		}
		if spec.AutoDiscoverable {
			for _, name := range spec.EnvAPIKeyNames {
				envNames[name] = struct{}{}
			}
		}
	}
	slices.Sort(providerTypes)

	// Configured providers may reference env vars that no built-in provider
	// auto-discovers; surface them so the UI shows their availability.
	providers, _ := raw["providers"].(map[string]any)
	for _, rawEntry := range providers {
		entry, _ := rawEntry.(map[string]any)
		if entry == nil {
			continue
		}
		if apiKey, _ := entry["api_key"].(string); apiKey != "" {
			if ref := IsAPIKeyEnvRef(apiKey); ref != "" {
				envNames[ref] = struct{}{}
			}
		}
	}
	env := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(envNames)) {
		env[name] = strings.TrimSpace(os.Getenv(name)) != ""
	}

	return map[string]any{
		"path":   path,
		"exists": exists,
		"config": presentConfig(raw, order),
		"options": map[string]any{
			"provider_types":    providerTypes,
			"permission_levels": PermissionLevelOptions(),
			"permission_modes":  PermissionModeOptions(),
			"reasoning_efforts": ReasoningEffortOptions,
		},
		"env":                          env,
		"provider_type_env_vars":       typeEnvVars,
		"provider_type_default_models": typeDefaultModels,
	}
}

// presentConfig redacts saved api_key strings (replacing them with the
// api_key_saved flag) and reshapes models into a sorted name list with
// optional overrides, so the UI never sees raw secrets.
func presentConfig(raw map[string]any, order ConfigOrder) map[string]any {
	out := maps.Clone(raw)
	if out == nil {
		out = map[string]any{}
	}
	providers, _ := raw["providers"].(map[string]any)
	if len(providers) == 0 {
		return out
	}
	presented := map[string]any{}
	for name, rawEntry := range providers {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			presented[name] = rawEntry
			continue
		}
		entry = maps.Clone(entry)
		apiKey := strings.TrimSpace(asString(entry["api_key"]))
		switch {
		case apiKey == "":
			entry["api_key"] = nil
			entry["api_key_saved"] = false
		case IsAPIKeyEnvRef(apiKey) != "":
			entry["api_key_saved"] = false
		default:
			entry["api_key"] = nil
			entry["api_key_saved"] = true
		}

		if models, _ := entry["models"].(map[string]any); len(models) > 0 {
			keys := orderedKeys(models, order.ModelOrder[name])
			entry["models"] = keys
			overrides := map[string]any{}
			for _, key := range keys {
				if modelOverride, _ := models[key].(map[string]any); len(modelOverride) > 0 {
					overrides[key] = modelOverride
				}
			}
			if len(overrides) > 0 {
				entry["model_overrides"] = overrides
			}
		}
		presented[name] = entry
	}
	out["providers"] = orderedMap{values: presented, order: order.ProviderOrder}
	return out
}

// MergeAPIKeys carries over saved api_keys for providers whose incoming
// payload omits them (the UI never echoes saved secrets back). Providers
// that explicitly clear api_key still drop it.
func MergeAPIKeys(incoming, existing map[string]any) {
	incomingProviders, _ := incoming["providers"].(map[string]any)
	existingProviders, _ := existing["providers"].(map[string]any)
	for name, rawEntry := range incomingProviders {
		entry, _ := rawEntry.(map[string]any)
		if entry == nil || entry["api_key"] != nil {
			continue
		}
		prior, _ := existingProviders[name].(map[string]any)
		if prior != nil {
			if apiKey, ok := prior["api_key"]; ok {
				entry["api_key"] = apiKey
				continue
			}
		}
		delete(entry, "api_key")
	}
}

// WriteSettingsFile writes payload as pretty-printed JSON via a temp-file
// rename, preserving the caller-supplied provider/model ordering. The atomic
// rename ensures a crash mid-write cannot truncate the settings.
func WriteSettingsFile(path string, payload map[string]any, order ConfigOrder) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".")
	if err != nil {
		return err
	}
	tmpName := file.Name()
	defer func() { _ = os.Remove(tmpName) }()

	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if providers, _ := payload["providers"].(map[string]any); len(providers) > 0 {
		ordered := map[string]any{}
		for _, name := range orderedKeys(providers, order.ProviderOrder) {
			rawEntry := providers[name]
			if entry, ok := rawEntry.(map[string]any); ok {
				entry = maps.Clone(entry)
				if models, _ := entry["models"].(map[string]any); len(models) > 0 {
					entry["models"] = orderedMap{values: models, order: order.ModelOrder[name]}
				}
				rawEntry = entry
			}
			ordered[name] = rawEntry
		}
		payload = maps.Clone(payload)
		payload["providers"] = orderedMap{values: ordered, order: order.ProviderOrder}
	}
	if err := encoder.Encode(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ModelsForProvider returns the models to expose for a resolved provider:
// configured models first (with their explicit order), otherwise the spec's
// defaults, falling back to the single resolved model when no spec matches.
func ModelsForProvider(settings Settings, resolved ResolvedProvider) []string {
	if pc, ok := settings.Providers[resolved.ProviderName]; ok && len(pc.Models) > 0 {
		if len(pc.ModelOrder) > 0 {
			return slices.Clone(pc.ModelOrder)
		}
		return slices.Sorted(maps.Keys(pc.Models))
	}
	spec, ok := provider.LookupSpec(resolved.ProviderType)
	if !ok || len(spec.DefaultModels) == 0 {
		return []string{resolved.Model}
	}
	return slices.Clone(spec.DefaultModels)
}

// orderedMap preserves provider/model insertion order when marshaling to
// JSON, so settings round-trip through the UI without churning keys.
type orderedMap struct {
	values map[string]any
	order  []string
}

func (m orderedMap) MarshalJSON() ([]byte, error) {
	var buf strings.Builder
	buf.WriteByte('{')
	for i, key := range orderedKeys(m.values, m.order) {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(m.values[key])
		if err != nil {
			return nil, err
		}
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return []byte(buf.String()), nil
}

// orderedKeys returns the keys of values in preferred order; remaining keys
// are appended alphabetically.
func orderedKeys(values map[string]any, preferred []string) []string {
	keys := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, key := range preferred {
		if _, ok := values[key]; !ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	return keys
}
