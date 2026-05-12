package models

import (
	"slices"
	"testing"
)

func TestLookupPrefersCurrentProviderFamily(t *testing.T) {
	withCatalog(t, `{
		"openai": {"gpt-5": {"max_output_tokens": 128000, "supports_reasoning": true, "supports_image_input": true}},
		"openrouter": {"openai/gpt-5": {"max_output_tokens": 64000, "supports_reasoning": true}}
	}`, func() {
		meta := Lookup("openrouter", "openai/gpt-5")
		if meta == nil || meta.Provider != "openrouter" || meta.MaxOutputTokens != 64000 {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupFallsBackToCanonicalProvider(t *testing.T) {
	withCatalog(t, `{
		"openai": {"gpt-5": {"max_output_tokens": 128000, "supports_reasoning": true, "supports_image_input": true}},
		"other": {}
	}`, func() {
		meta := Lookup("openai_chat", "openai/gpt-5")
		if meta == nil || meta.Provider != "openai_chat" || meta.Model != "openai/gpt-5" {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
		if meta.SupportsImageInput == nil || !*meta.SupportsImageInput {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupFallsBackToOpenRouterSuffix(t *testing.T) {
	withCatalog(t, `{
		"moonshotai": {},
		"openrouter": {"moonshotai/kimi-k2.6": {"max_output_tokens": 262144, "supports_reasoning": true}}
	}`, func() {
		meta := Lookup("moonshotai", "kimi-k2.6")
		if meta == nil || meta.Provider != "moonshotai" || meta.Model != "kimi-k2.6" || meta.MaxOutputTokens != 262144 {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
		if meta.SupportsReasoning == nil || !*meta.SupportsReasoning {
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
		if meta := Lookup("moonshotai", "shared-model"); meta != nil {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupRequiresProvider(t *testing.T) {
	withCatalog(t, `{
		"openai": {"gpt-5": {"max_output_tokens": 128000}},
		"openrouter": {"some-provider/some-niche-model": {"max_output_tokens": 64000}}
	}`, func() {
		if meta := Lookup("", "some-niche-model"); meta != nil {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
		if meta := Lookup("", "gpt-5"); meta != nil {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupReadsImageAndPDFSupport(t *testing.T) {
	withCatalog(t, `{
		"openai": {
			"gpt-5.4": {
				"max_output_tokens": 128000,
				"supports_reasoning": true,
				"supports_image_input": true,
				"supports_pdf_input": true
			}
		}
	}`, func() {
		meta := Lookup("openai", "gpt-5.4")
		if meta == nil || meta.SupportsImageInput == nil || meta.SupportsPDFInput == nil {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
		if !*meta.SupportsImageInput || !*meta.SupportsPDFInput {
			t.Fatalf("unexpected metadata: %#v", meta)
		}
	})
}

func TestLookupDoesNotRetryAfterFirstCatalogLoad(t *testing.T) {
	withCatalog(t, `{"zai": {}}`, func() {
		if meta := Lookup("zai", "glm-5.1"); meta != nil {
			t.Fatalf("unexpected metadata: %#v", meta)
		}

		catalogJSON = []byte(`{"openrouter": {"zai/glm-5.1": {"max_output_tokens": 131072}}}`)
		if meta := Lookup("zai", "glm-5.1"); meta != nil {
			t.Fatalf("unexpected metadata after cached miss: %#v", meta)
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
