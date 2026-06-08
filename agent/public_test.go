package agent_test

import (
	"testing"

	"github.com/legibet/mycode-go/agent"
)

func TestPublicAgentTemperatureValidation(t *testing.T) {
	if _, err := agent.New(agent.Config{
		Provider:    "anthropic",
		Model:       "claude-opus-4-7",
		CWD:         t.TempDir(),
		Temperature: new(1.5),
	}); err == nil {
		t.Fatal("expected out-of-range temperature error")
	}

	if _, err := agent.New(agent.Config{
		Provider:        "anthropic",
		Model:           "claude-opus-4-7",
		CWD:             t.TempDir(),
		ReasoningEffort: "high",
		Temperature:     new(0.5),
	}); err == nil {
		t.Fatal("expected thinking+temperature error")
	}
}

func TestPublicAgentInfersProviderFromModel(t *testing.T) {
	runtime, err := agent.New(agent.Config{
		Model: "claude-opus-4-7",
		CWD:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("agent.New returned error: %v", err)
	}
	if runtime.Provider != "anthropic" {
		t.Fatalf("inferred provider = %q, want anthropic", runtime.Provider)
	}
}
