package provider

import (
	"cmp"
	"encoding/json"
	"strings"
	"sync"

	_ "embed"
)

//go:embed models_catalog.json
var catalogJSON []byte

// ModelMetadata is the resolved capability set for a model. Fields the catalog
// does not specify stay at their zero value.
type ModelMetadata struct {
	ContextWindow      int  `json:"context_window"`
	MaxOutputTokens    int  `json:"max_output_tokens"`
	SupportsReasoning  bool `json:"supports_reasoning"`
	SupportsImageInput bool `json:"supports_image_input"`
	SupportsPDFInput   bool `json:"supports_pdf_input"`
}

// ModelOverride carries optional per-model overrides layered on top of the
// bundled catalog. A zero int or nil pointer means "not overridden".
type ModelOverride struct {
	ContextWindow      int
	MaxOutputTokens    int
	SupportsReasoning  *bool
	SupportsImageInput *bool
	SupportsPDFInput   *bool
}

// ResolveModel returns catalog metadata for (providerType, model) with non-zero
// override fields layered on top. Unknown values stay at zero; callers apply
// their own defaults.
func ResolveModel(providerType, model string, ov ModelOverride) ModelMetadata {
	meta := lookupModel(providerType, model)
	return ModelMetadata{
		ContextWindow:      cmp.Or(ov.ContextWindow, meta.ContextWindow),
		MaxOutputTokens:    cmp.Or(ov.MaxOutputTokens, meta.MaxOutputTokens),
		SupportsReasoning:  boolOr(ov.SupportsReasoning, meta.SupportsReasoning),
		SupportsImageInput: boolOr(ov.SupportsImageInput, meta.SupportsImageInput),
		SupportsPDFInput:   boolOr(ov.SupportsPDFInput, meta.SupportsPDFInput),
	}
}

type catalogMap map[string]map[string]ModelMetadata

var loadCatalog = newCatalogLoader()

func newCatalogLoader() func() catalogMap {
	return sync.OnceValue(func() catalogMap {
		var parsed catalogMap
		if err := json.Unmarshal(catalogJSON, &parsed); err != nil {
			return nil
		}
		return parsed
	})
}

// lookupModel resolves catalog capabilities for a provider/model request,
// trying the exact pair, then the inferred provider with the unprefixed name,
// then a unique openrouter suffix match. Returns the zero value when unknown.
func lookupModel(providerType, model string) ModelMetadata {
	providerType = strings.TrimSpace(providerType)
	requestedModel := strings.TrimSpace(model)
	if providerType == "" || requestedModel == "" {
		return ModelMetadata{}
	}
	catalog := loadCatalog()
	if len(catalog) == 0 {
		return ModelMetadata{}
	}

	modelName := requestedModel
	if _, after, ok := strings.Cut(requestedModel, "/"); ok {
		modelName = strings.TrimSpace(after)
	}

	if entry, ok := catalog[providerType][requestedModel]; ok {
		return entry
	}
	if inferred, ok := InferProviderFromModel(modelName); ok && inferred != providerType {
		if entry, ok := catalog[inferred][modelName]; ok {
			return entry
		}
	}

	var match ModelMetadata
	found := false
	for modelID, entry := range catalog["openrouter"] {
		_, suffix, ok := strings.Cut(modelID, "/")
		if !ok || strings.TrimSpace(suffix) != modelName {
			continue
		}
		if found {
			return ModelMetadata{} // ambiguous suffix — don't guess
		}
		match, found = entry, true
	}
	if found {
		return match
	}
	return ModelMetadata{}
}

func boolOr(override *bool, fallback bool) bool {
	if override != nil {
		return *override
	}
	return fallback
}
