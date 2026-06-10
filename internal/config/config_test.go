package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/legibet/mycode-go/provider"
)

func TestLoadMergesGlobalAndCurrentDirectoryConfigs(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	cwd := filepath.Join(root, "project", "apps", "api")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)

	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"shared": {
				"type": "openai",
				"api_key": "global-key",
				"models": {"gpt-5-mini": {}}
			}
		},
		"default": {
			"provider": "shared",
			"model": "gpt-5-mini"
		}
	}`)
	writeJSON(t, filepath.Join(cwd, ".mycode", "config.json"), `{
		"default": {
			"provider": "shared",
			"model": "gpt-5.4"
		},
		"providers": {
			"shared": {
				"base_url": "https://root.example/v1",
				"models": {"gpt-5.4": {}}
			}
		}
	}`)

	settings, err := Load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if settings.CWD != ResolveSymlinks(cwd) {
		t.Fatalf("unexpected settings: %#v", settings)
	}
	if settings.DefaultProvider != "shared" || settings.DefaultModel != "gpt-5.4" {
		t.Fatalf("unexpected defaults: %#v", settings)
	}
	if settings.Providers["shared"].APIKey != "global-key" || settings.Providers["shared"].BaseURL != "https://root.example/v1" {
		t.Fatalf("unexpected provider: %#v", settings.Providers["shared"])
	}
	if len(settings.Providers["shared"].Models) != 1 {
		t.Fatalf("unexpected models: %#v", settings.Providers["shared"].Models)
	}
}

func TestLoadCompactThresholdParsing(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if settings.CompactThreshold != defaultCompactThreshold {
		t.Fatalf("unexpected default compact threshold: %v", settings.CompactThreshold)
	}

	writeJSON(t, filepath.Join(home, "config.json"), `{"default":{"compact_threshold":false}}`)
	settings, err = Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if settings.CompactThreshold != 0 {
		t.Fatalf("unexpected compact threshold: %v", settings.CompactThreshold)
	}
}

func TestLoadMergesGlobalAndProjectConfigsFromProjectToCwd(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	project := filepath.Join(root, "project")
	cwd := filepath.Join(project, "apps", "api")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)

	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"openai": {"api_key": "global-key"}
		}
	}`)
	writeJSON(t, filepath.Join(project, ".mycode", "config.json"), `{
		"providers": {
			"openai": {"base_url": "https://root.example/v1"}
		}
	}`)
	writeJSON(t, filepath.Join(cwd, ".mycode", "config.json"), `{
		"default": {"provider": "openai", "model": "gpt-4.1"},
		"providers": {
			"openai": {"models": {"gpt-4.1": {}}}
		}
	}`)

	settings, err := Load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if settings.CWD != ResolveSymlinks(cwd) {
		t.Fatalf("expected cwd %q, got %q", ResolveSymlinks(cwd), settings.CWD)
	}
	if settings.Project != ResolveSymlinks(project) {
		t.Fatalf("expected project %q, got %q", ResolveSymlinks(project), settings.Project)
	}
	if settings.DefaultProvider != "openai" || settings.DefaultModel != "gpt-4.1" {
		t.Fatalf("unexpected default: provider=%q model=%q", settings.DefaultProvider, settings.DefaultModel)
	}
	if p := settings.Providers["openai"]; p.APIKey != "global-key" || p.BaseURL != "https://root.example/v1" || len(p.Models) != 1 {
		t.Fatalf("unexpected provider: %#v", p)
	}
	expectedPaths := []string{
		ResolveSymlinks(filepath.Join(home, "config.json")),
		ResolveSymlinks(filepath.Join(project, ".mycode", "config.json")),
		ResolveSymlinks(filepath.Join(cwd, ".mycode", "config.json")),
	}
	if !slices.Equal(settings.ConfigPaths, expectedPaths) {
		t.Fatalf("unexpected config paths: %#v, want %#v", settings.ConfigPaths, expectedPaths)
	}
}

func TestLoadTreatsGitFileAsProjectRoot(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	project := filepath.Join(root, "project")
	cwd := filepath.Join(project, "apps", "api")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)

	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("gitdir: ../.git/worktrees/project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(project, ".mycode", "config.json"), `{"default": {"provider": "parent"}}`)
	writeJSON(t, filepath.Join(cwd, ".mycode", "config.json"), `{"default": {"provider": "local"}}`)

	settings, err := Load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Project != ResolveSymlinks(project) {
		t.Fatalf("expected project %q, got %q", ResolveSymlinks(project), settings.Project)
	}
	if settings.DefaultProvider != "local" {
		t.Fatalf("expected local to win, got %q", settings.DefaultProvider)
	}
	expectedPaths := []string{
		ResolveSymlinks(filepath.Join(project, ".mycode", "config.json")),
		ResolveSymlinks(filepath.Join(cwd, ".mycode", "config.json")),
	}
	if !slices.Equal(settings.ConfigPaths, expectedPaths) {
		t.Fatalf("unexpected config paths: %#v, want %#v", settings.ConfigPaths, expectedPaths)
	}
}

func TestLoadUsesCwdAsProjectWhenNoGit(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	cwd := filepath.Join(project, "apps", "api")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(project, ".mycode", "config.json"), `{"default": {"provider": "parent"}}`)
	writeJSON(t, filepath.Join(cwd, ".mycode", "config.json"), `{"default": {"provider": "local"}}`)

	settings, err := Load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Project != ResolveSymlinks(cwd) {
		t.Fatalf("expected project==cwd when no .git, got %q vs %q", settings.Project, settings.CWD)
	}
	if settings.DefaultProvider != "local" {
		t.Fatalf("expected local to win, got %q", settings.DefaultProvider)
	}
}

func TestLoadPermissionConfig(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Permission != (PermissionConfig{Level: "safe", Mode: "ask"}) {
		t.Fatalf("unexpected default permission: %#v", settings.Permission)
	}

	writeJSON(t, filepath.Join(home, "config.json"), `{"permission":{"level":"readonly","mode":"deny"}}`)
	settings, err = Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Permission != (PermissionConfig{Level: "readonly", Mode: "deny"}) {
		t.Fatalf("unexpected permission: %#v", settings.Permission)
	}
}

func TestLoadPermissionStringKeepsCurrentMode(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, ".mycode"), 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)

	writeJSON(t, filepath.Join(home, "config.json"), `{"permission":{"level":"standard","mode":"deny"}}`)
	writeJSON(t, filepath.Join(workspace, ".mycode", "config.json"), `{"permission":"readonly"}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Permission != (PermissionConfig{Level: "readonly", Mode: "deny"}) {
		t.Fatalf("unexpected permission: %#v", settings.Permission)
	}
}

func TestResolveProviderAutoDiscoveryOrder(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENAI_API_KEY", "openai-env-key")
	t.Setenv("MOONSHOT_API_KEY", "moonshot-env-key")

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "openai" || resolved.ProviderType != "openai" {
		t.Fatalf("unexpected provider: %#v", resolved)
	}
}

func TestResolveProviderAutoDiscoveryMatchesRegistryOrder(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("GOOGLE_API_KEY", "google-env-key")
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-env-key")

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "google" || resolved.ProviderType != "google" {
		t.Fatalf("unexpected provider: %#v", resolved)
	}
}

func TestResolveProviderPrefersFirstConfiguredProviderWithCredentials(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENROUTER_API_KEY", "router-env-key")
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-env-key")

	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"openrouter": {
				"models": {"openai/gpt-5": {}}
			},
			"deepseek": {
				"models": {"deepseek-chat": {}}
			}
		}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "openrouter" || resolved.ProviderType != "openrouter" || resolved.APIKey != "router-env-key" {
		t.Fatalf("unexpected provider: %#v", resolved)
	}
}

func TestResolveProviderDeepSeekDefaultsToV4(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-env-key")

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "deepseek", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "deepseek" || resolved.ProviderType != "deepseek" {
		t.Fatalf("unexpected provider: %#v", resolved)
	}
	if resolved.Model != "deepseek-v4-pro" {
		t.Fatalf("unexpected DeepSeek defaults: %#v", resolved)
	}
}

func TestResolveProviderPreservesConfiguredModelOrder(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENAI_API_KEY", "env-key")

	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"openai": {
				"models": {
					"z-model": {},
					"a-model": {}
				}
			}
		},
		"default": {"provider": "openai"}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.Providers["openai"].ModelOrder; len(got) != 2 || got[0] != "z-model" || got[1] != "a-model" {
		t.Fatalf("unexpected model order: %#v", got)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model != "z-model" {
		t.Fatalf("unexpected model: %#v", resolved)
	}
}

func TestResolveProviderPrefersExplicitAPIKeyOverEnv(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("ANTHROPIC_API_KEY", "env-key")

	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"claude": {
				"type": "anthropic",
				"api_key": "config-key",
				"models": {"claude-sonnet-4-6": {}}
			}
		},
		"default": {"provider": "claude"}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "request-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.APIKey != "request-key" {
		t.Fatalf("unexpected api key: %#v", resolved)
	}
}

func TestResolveProviderOpenAIChatIgnoresReasoningEffort(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENROUTER_API_KEY", "router-env-key")
	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"router": {
				"type": "openai_chat",
				"api_key": "${OPENROUTER_API_KEY}",
				"base_url": "https://openrouter.ai/api/v1",
				"models": {"openai/gpt-5": {}},
				"reasoning_effort": "high"
			}
		},
		"default": {"provider": "router"}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderType != "openai_chat" || resolved.ReasoningEffort != "" {
		t.Fatalf("unexpected provider: %#v", resolved)
	}
}

func TestResolveProviderFallsBackWhenDefaultProviderFails(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENAI_API_KEY", "openai-env-key")
	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"claude": {
				"type": "anthropic",
				"models": {"claude-sonnet-4-6": {}}
			}
		},
		"default": {"provider": "claude"}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "openai" || resolved.APIKey != "openai-env-key" {
		t.Fatalf("unexpected provider: %#v", resolved)
	}
}

func TestResolveProviderExplicitProviderNameDoesNotFallback(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENAI_API_KEY", "openai-env-key")
	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"claude": {
				"type": "anthropic",
				"models": {"claude-sonnet-4-6": {}}
			}
		},
		"default": {"provider": "openai"}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveProvider(settings, "claude", "", "", ""); err == nil || !contains(err.Error(), `provider "claude" is selected`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsCustomProviderWithoutType(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"custom-provider": {
				"base_url": "https://custom-endpoint.example/v1"
			}
		}
	}`)

	if _, err := Load(workspace); err == nil || !contains(err.Error(), `provider "custom-provider" must set 'type'`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveProviderFallsBackWhenDefaultProviderAPIKeyEnvVarIsMissing(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENAI_API_KEY", "default-env-key")
	t.Setenv("OPENROUTER_API_KEY", "")
	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"router": {
				"type": "openai_chat",
				"api_key": "${OPENROUTER_API_KEY}",
				"base_url": "https://openrouter.ai/api/v1",
				"models": {"openai/gpt-5": {}}
			}
		},
		"default": {"provider": "router"}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "openai" || resolved.APIKey != "default-env-key" {
		t.Fatalf("unexpected provider: %#v", resolved)
	}
}

func TestResolveProviderUsesMetadataOverrides(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENAI_API_KEY", "env-key")

	writeJSON(t, filepath.Join(home, "config.json"), `{
		"providers": {
			"openai": {
				"models": {
					"gpt-5.4": {
						"context_window": 500000,
						"max_output_tokens": 64000,
						"supports_reasoning": false,
						"supports_image_input": true
					}
				}
			}
		},
		"default": {"provider": "openai"}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	meta := resolved.Metadata
	if meta.ContextWindow != 500000 || meta.MaxOutputTokens != 64000 || meta.SupportsReasoning || !meta.SupportsImageInput {
		t.Fatalf("unexpected resolved metadata: %#v", meta)
	}
}

func TestResolveProviderUsesGlobalDefaultReasoningEffort(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", ".mycode")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateConfigTest(t, home)
	t.Setenv("OPENAI_API_KEY", "env-key")

	writeJSON(t, filepath.Join(home, "config.json"), `{
		"default": {
			"provider": "openai",
			"reasoning_effort": "high"
		},
		"providers": {
			"openai": {
				"models": {
					"gpt-5.4": {
						"supports_reasoning": true
					}
				}
			}
		}
	}`)

	settings, err := Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProvider(settings, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderType != "openai" || resolved.ReasoningEffort != "high" {
		t.Fatalf("unexpected resolved provider: %#v", resolved)
	}
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(text, needle string) bool {
	return strings.Contains(text, needle)
}

func isolateConfigTest(t *testing.T, home string) {
	t.Helper()
	t.Setenv("MYCODE_HOME", home)
	t.Setenv("PORT", "")
	for _, spec := range provider.Specs() {
		for _, envName := range spec.EnvAPIKeyNames {
			t.Setenv(envName, "")
		}
	}
}
