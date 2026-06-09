package provider

import (
	"slices"
	"testing"
)

func TestLookupPrefersExactProviderEntry(t *testing.T) {
	withCatalog(t, `{
		"openai": {"gpt-5": {"max_output_tokens": 128000, "supports_reasoning": true, "supports_image_input": true}},
		"openrouter": {"openai/gpt-5": {"max_output_tokens": 64000, "supports_reasoning": true}}
	}`, func() {
		meta := lookupModel("openrouter", "openai/gpt-5")
		if meta.MaxOutputTokens != 64000 {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupFallsBackToInferredProvider(t *testing.T) {
	withCatalog(t, `{
		"openai": {"gpt-5": {"max_output_tokens": 128000, "supports_reasoning": true, "supports_image_input": true}},
		"other": {}
	}`, func() {
		meta := lookupModel("openai_chat", "openai/gpt-5")
		if !meta.SupportsImageInput || meta.MaxOutputTokens != 128000 {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupFallsBackToOpenRouterSuffix(t *testing.T) {
	withCatalog(t, `{
		"moonshotai": {},
		"openrouter": {"moonshotai/kimi-k2.6": {"max_output_tokens": 262144, "supports_reasoning": true}}
	}`, func() {
		meta := lookupModel("moonshotai", "kimi-k2.6")
		if meta.MaxOutputTokens != 262144 || !meta.SupportsReasoning {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupOpenRouterSuffixRejectsAmbiguousMatches(t *testing.T) {
	withCatalog(t, `{
		"openrouter": {
			"first/shared-model": {"max_output_tokens": 64000},
			"second/shared-model": {"max_output_tokens": 128000}
		}
	}`, func() {
		if meta := lookupModel("moonshotai", "shared-model"); meta != (ModelMetadata{}) {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupRequiresProvider(t *testing.T) {
	withCatalog(t, `{
		"openai": {"gpt-5": {"max_output_tokens": 128000}},
		"openrouter": {"some-provider/some-niche-model": {"max_output_tokens": 64000}}
	}`, func() {
		if meta := lookupModel("", "some-niche-model"); meta != (ModelMetadata{}) {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
		if meta := lookupModel("", "gpt-5"); meta != (ModelMetadata{}) {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestResolveModelLayersOverrides(t *testing.T) {
	withCatalog(t, `{
		"openai": {"gpt-5.4": {"context_window": 400000, "max_output_tokens": 128000, "supports_image_input": true}}
	}`, func() {
		disabled := false
		meta := ResolveModel("openai", "gpt-5.4", ModelOverride{
			MaxOutputTokens:    32000,
			SupportsImageInput: &disabled,
		})
		if meta.MaxOutputTokens != 32000 {
			t.Fatalf("override did not win: %#v", meta)
		}
		if meta.ContextWindow != 400000 {
			t.Fatalf("catalog fallback lost: %#v", meta)
		}
		if meta.SupportsImageInput {
			t.Fatalf("override should disable image input: %#v", meta)
		}
	})
}

func withCatalog(t *testing.T, raw string, fn func()) {
	t.Helper()

	originalJSON := slices.Clone(catalogJSON)
	originalLoad := loadCatalog
	catalogJSON = []byte(raw)
	loadCatalog = newCatalogLoader()

	t.Cleanup(func() {
		catalogJSON = originalJSON
		loadCatalog = originalLoad
	})

	fn()
}
