package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"iter"
	"os"
	"strings"

	agentpkg "github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/permissions"
	promptpkg "github.com/legibet/mycode-go/internal/prompt"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/session"
	"github.com/legibet/mycode-go/tools"
)

type resolvedSession struct {
	ID string
}

func runCommand(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	providerName := fs.String("provider", "", "Provider id or configured alias")
	model := fs.String("model", "", "Model name")
	maxTurns := fs.Int("max-turns", 0, "Maximum tool loop turns")
	sessionID := fs.String("session", "", "Resume a specific session id")
	continueLast := fs.Bool("continue", false, "Resume the latest session in the current workspace")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *sessionID != "" && *continueLast {
		_, _ = fmt.Fprintln(os.Stderr, "--session and --continue are mutually exclusive")
		return 2
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		_, _ = fmt.Fprintln(os.Stderr, "message is required")
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	settings, err := config.Load(cwd)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resolvedProvider, err := config.ResolveProvider(settings, *providerName, *model, "", "")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}

	store, err := session.NewStore(config.ResolveSessionsDir())
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resolvedSession, err := resolveSession(
		store,
		cwd,
		*sessionID,
		*continueLast,
	)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}

	agent, err := agentpkg.New(agentpkg.Config{
		Model:            resolvedProvider.Model,
		Provider:         resolvedProvider.ProviderType,
		CWD:              cwd,
		Store:            store,
		SessionID:        resolvedSession.ID,
		APIKey:           resolvedProvider.APIKey,
		APIBase:          resolvedProvider.APIBase,
		System:           promptpkg.Build(cwd, settings.Project, config.ResolveHome()),
		MaxTurns:         *maxTurns,
		Metadata:         &resolvedProvider.Metadata,
		CompactThreshold: settings.CompactThreshold,
		DisableCompact:   settings.CompactThreshold <= 0,
		ReasoningEffort:  resolvedProvider.ReasoningEffort,
		Tools:            []tools.Spec{tools.Read, tools.Write, tools.Edit, tools.Bash},
		Hooks: tools.Hooks{
			BeforeTool: []tools.BeforeToolHook{
				permissions.ToolHook(
					settings.Permission,
					nil,
					cwd,
					settings.Project,
					permissions.SkillRoots(cwd, settings.Project, config.ResolveHome()),
				),
			},
		},
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}

	userMessage := message.UserTextMessage(prompt, nil)
	reply, errorMessage := runNoninteractive(context.Background(), agent, userMessage)
	if reply != "" {
		_, _ = fmt.Fprintln(os.Stdout, reply)
	}
	if errorMessage != "" {
		if reply == "" {
			_, _ = fmt.Fprintln(os.Stderr, errorMessage)
		}
		return 1
	}
	return 0
}

// chatAgent is the agent behavior runNoninteractive needs.
type chatAgent interface {
	ChatMessage(ctx context.Context, msg message.Message) iter.Seq[agentpkg.Event]
}

// runNoninteractive drives one turn and returns the final assistant text plus
// an error message (empty when none). The agent persists on its own; here we
// only collect the reply from the event stream.
func runNoninteractive(ctx context.Context, agent chatAgent, userMessage message.Message) (string, string) {
	var reply strings.Builder
	errorMessage := ""
	for event := range agent.ChatMessage(ctx, userMessage) {
		switch event.Type {
		case "text":
			if delta, ok := event.Data["delta"].(string); ok {
				reply.WriteString(delta)
			}
		case "tool_start":
			// A turn that calls tools is not the final answer; drop its text.
			reply.Reset()
		case "error":
			if text, ok := event.Data["message"].(string); ok && text != "" {
				errorMessage = text
			} else {
				errorMessage = "agent error"
			}
		case "tool_done":
			if event.Data["is_error"] != true {
				continue
			}
			output, _ := event.Data["output"].(string)
			if output == permissions.DeniedOutput || output == permissions.DeniedByUserOutput {
				errorMessage = output
			}
		}
	}
	return strings.TrimSpace(reply.String()), errorMessage
}

func resolveSession(store *session.Store, cwd, requestedSessionID string, continueLast bool) (resolvedSession, error) {
	if requestedSessionID != "" {
		data, err := store.LoadSession(requestedSessionID)
		if err != nil {
			return resolvedSession{}, err
		}
		if data == nil {
			return resolvedSession{}, fmt.Errorf("unknown session: %s", requestedSessionID)
		}
		return resolvedSession{ID: requestedSessionID}, nil
	}

	if continueLast {
		latest, err := store.LatestSession(cwd)
		if err != nil {
			return resolvedSession{}, err
		}
		if latest != nil && latest.ID != "" {
			data, err := store.LoadSession(latest.ID)
			if err != nil {
				return resolvedSession{}, err
			}
			if data == nil {
				return resolvedSession{}, fmt.Errorf("unknown session: %s", latest.ID)
			}
			return resolvedSession{ID: latest.ID}, nil
		}
	}

	draft := store.DraftSession(cwd)
	return resolvedSession{ID: draft.Session.ID}, nil
}
