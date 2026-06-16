package provider

import "strings"

// Spec is the static metadata for one built-in provider type.
type Spec struct {
	ID                      string
	Label                   string
	DefaultBaseURL          string
	EnvAPIKeyNames          []string
	DefaultModels           []string
	SupportsReasoningEffort bool
	AutoDiscoverable        bool
}

var specs = []Spec{
	{
		ID:                      "anthropic",
		Label:                   "Anthropic Messages",
		DefaultBaseURL:          "https://api.anthropic.com",
		EnvAPIKeyNames:          []string{"ANTHROPIC_API_KEY"},
		DefaultModels:           []string{"claude-sonnet-4-6", "claude-opus-4-8"},
		SupportsReasoningEffort: true,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "openai",
		Label:                   "OpenAI Responses",
		DefaultBaseURL:          "https://api.openai.com/v1",
		EnvAPIKeyNames:          []string{"OPENAI_API_KEY"},
		DefaultModels:           []string{"gpt-5.5", "gpt-5.4-mini"},
		SupportsReasoningEffort: true,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "google",
		Label:                   "Google Gemini",
		DefaultBaseURL:          "https://generativelanguage.googleapis.com",
		EnvAPIKeyNames:          []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		DefaultModels:           []string{"gemini-3.5-flash", "gemini-3.1-pro-preview"},
		SupportsReasoningEffort: true,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "deepseek",
		Label:                   "DeepSeek Chat Completions",
		DefaultBaseURL:          "https://api.deepseek.com",
		EnvAPIKeyNames:          []string{"DEEPSEEK_API_KEY"},
		DefaultModels:           []string{"deepseek-v4-pro", "deepseek-v4-flash"},
		SupportsReasoningEffort: true,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "zai",
		Label:                   "Z.AI Chat Completions",
		DefaultBaseURL:          "https://api.z.ai/api/paas/v4/",
		EnvAPIKeyNames:          []string{"ZAI_API_KEY"},
		DefaultModels:           []string{"glm-5.2"},
		SupportsReasoningEffort: true,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "moonshotai",
		Label:                   "Moonshot Anthropic-Compatible",
		DefaultBaseURL:          "https://api.moonshot.ai/anthropic",
		EnvAPIKeyNames:          []string{"MOONSHOT_API_KEY"},
		DefaultModels:           []string{"kimi-k2.7-code", "kimi-k2.6"},
		SupportsReasoningEffort: true,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "minimax",
		Label:                   "MiniMax Anthropic-Compatible",
		DefaultBaseURL:          "https://api.minimax.io/anthropic",
		EnvAPIKeyNames:          []string{"MINIMAX_API_KEY"},
		DefaultModels:           []string{"MiniMax-M3"},
		SupportsReasoningEffort: false,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "openrouter",
		Label:                   "OpenRouter Chat Completions",
		DefaultBaseURL:          "https://openrouter.ai/api/v1",
		EnvAPIKeyNames:          []string{"OPENROUTER_API_KEY"},
		DefaultModels:           []string{"openrouter/auto"},
		SupportsReasoningEffort: true,
		AutoDiscoverable:        true,
	},
	{
		ID:                      "openai_chat",
		Label:                   "OpenAI Chat Completions",
		DefaultBaseURL:          "https://api.openai.com/v1",
		EnvAPIKeyNames:          []string{"OPENAI_API_KEY"},
		SupportsReasoningEffort: false,
		AutoDiscoverable:        false,
	},
}

var specByID = func() map[string]Spec {
	byID := make(map[string]Spec, len(specs))
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	return byID
}()

// Specs returns all built-in provider specs.
func Specs() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

// LookupSpec returns one built-in provider spec.
func LookupSpec(id string) (Spec, bool) {
	spec, ok := specByID[id]
	return spec, ok
}

var adapters = map[string]Adapter{
	"anthropic":   newAnthropicAdapter("anthropic"),
	"moonshotai":  newAnthropicAdapter("moonshotai"),
	"minimax":     newAnthropicAdapter("minimax"),
	"openai":      newOpenAIResponsesAdapter(),
	"openai_chat": newOpenAIChatAdapter("openai_chat"),
	"deepseek":    newOpenAIChatAdapter("deepseek"),
	"zai":         newOpenAIChatAdapter("zai"),
	"openrouter":  newOpenAIChatAdapter("openrouter"),
	"google":      newGoogleAdapter(),
}

// LookupAdapter returns one registered provider adapter.
func LookupAdapter(id string) (Adapter, bool) {
	adapter, ok := adapters[id]
	return adapter, ok
}

// InferProviderFromModel returns the built-in provider id for a known model id.
// A leading "vendor/" prefix is dropped before matching, so both "claude-..."
// and "anthropic/claude-..." resolve to "anthropic".
func InferProviderFromModel(model string) (string, bool) {
	bare := strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	switch {
	case strings.HasPrefix(bare, "claude-"):
		return "anthropic", true
	case strings.HasPrefix(bare, "deepseek-"):
		return "deepseek", true
	case strings.HasPrefix(bare, "gemini-"):
		return "google", true
	case strings.HasPrefix(bare, "glm-"):
		return "zai", true
	case strings.HasPrefix(bare, "gpt-"), strings.HasPrefix(bare, "o1"), strings.HasPrefix(bare, "o3"), strings.HasPrefix(bare, "o4"):
		return "openai", true
	case strings.HasPrefix(bare, "kimi-"):
		return "moonshotai", true
	case strings.HasPrefix(bare, "minimax-"):
		return "minimax", true
	default:
		return "", false
	}
}
