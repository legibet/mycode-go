package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentpkg "github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/attachment"
	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/permissions"
	promptpkg "github.com/legibet/mycode-go/internal/prompt"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/session"
	"github.com/legibet/mycode-go/tools"
)

type resolvedSession struct {
	ID       string
	Messages []message.Message
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

	store := session.NewStore(config.ResolveSessionsDir())
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
		Model:              resolvedProvider.Model,
		Provider:           resolvedProvider.ProviderType,
		CWD:                cwd,
		SessionDir:         store.SessionDir(resolvedSession.ID),
		SessionID:          resolvedSession.ID,
		APIKey:             resolvedProvider.APIKey,
		APIBase:            resolvedProvider.APIBase,
		System:             promptpkg.Build(cwd, settings.Project, config.ResolveHome()),
		Messages:           resolvedSession.Messages,
		MaxTurns:           *maxTurns,
		MaxTokens:          resolvedProvider.MaxTokens,
		ContextWindow:      resolvedProvider.ContextWindow,
		CompactThreshold:   settings.CompactThreshold,
		ReasoningEffort:    resolvedProvider.ReasoningEffort,
		SupportsImageInput: resolvedProvider.SupportsImageInput,
		SupportsPDFInput:   resolvedProvider.SupportsPDFInput,
		ToolSpecs:          tools.DefaultSpecs(),
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

	var latestAssistant *message.Message
	onPersist := func(msg message.Message) error {
		if msg.Role == "assistant" {
			latestAssistant = new(message.Clone(msg))
		}
		return store.AppendMessage(resolvedSession.ID, msg, cwd)
	}

	userMessage := buildRunUserMessage(prompt, cwd)
	errorMessage := ""
	for event := range agent.Chat(context.Background(), userMessage, agentpkg.ChatOptions{OnPersist: onPersist}) {
		if event.Type != "error" {
			continue
		}
		if text, ok := event.Data["message"].(string); ok && text != "" {
			errorMessage = text
		} else {
			errorMessage = "agent error"
		}
	}
	if errorMessage != "" {
		_, _ = fmt.Fprintln(os.Stderr, errorMessage)
		return 1
	}

	reply := flattenAssistantReply(latestAssistant)
	if reply != "" {
		_, _ = fmt.Fprintln(os.Stdout, reply)
	}
	return 0
}

func buildRunUserMessage(prompt, cwd string) message.Message {
	blocks := []message.Block{message.TextBlock(prompt, nil)}
	seen := map[string]struct{}{}
	for _, pathText := range runAttachmentPaths(prompt) {
		resolved := tools.ResolvePath(pathText, cwd)
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}

		item := attachment.Path(resolved)
		if tools.DetectImageMIMEType(resolved) == "" && tools.DetectDocumentMIMEType(resolved) == "" {
			item = attachment.PathWithName(resolved, resolved)
		}
		attachmentBlocks, err := attachment.Build([]attachment.Attachment{item}, attachment.Options{CWD: cwd})
		if err != nil {
			continue
		}
		blocks = append(blocks, attachmentBlocks...)
	}
	return message.BuildMessage("user", blocks, nil)
}

func runAttachmentPaths(prompt string) []string {
	tokens := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(prompt, "\r\n", "\n"), "\r", "\n"))
	paths := make([]string, 0)
	for _, token := range tokens {
		if !strings.HasPrefix(token, "@") || token == "@" {
			continue
		}
		path := strings.Trim(token[1:], `"'`)
		if path != "" {
			paths = append(paths, filepath.Clean(path))
		}
	}
	return paths
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
		return resolvedSession{ID: requestedSessionID, Messages: data.Messages}, nil
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
			return resolvedSession{ID: latest.ID, Messages: data.Messages}, nil
		}
	}

	draft := store.DraftSession(cwd)
	return resolvedSession{ID: draft.Session.ID, Messages: nil}, nil
}

func flattenAssistantReply(msg *message.Message) string {
	if msg == nil {
		return ""
	}
	parts := make([]string, 0, len(msg.Content))
	for _, block := range msg.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}
