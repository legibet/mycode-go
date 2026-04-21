package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	agentpkg "github.com/legibet/mycode-go/internal/agent"
	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/provider"
	"github.com/legibet/mycode-go/internal/tools"
)

type chatInputBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	Path         string `json:"path"`
	Data         string `json:"data"`
	MIMEType     string `json:"mime_type"`
	Name         string `json:"name"`
	IsAttachment bool   `json:"is_attachment"`
}

type chatRequest struct {
	SessionID       string           `json:"session_id"`
	Message         string           `json:"message"`
	Input           []chatInputBlock `json:"input"`
	Provider        string           `json:"provider"`
	Model           string           `json:"model"`
	CWD             string           `json:"cwd"`
	APIKey          string           `json:"api_key"`
	APIBase         string           `json:"api_base"`
	ReasoningEffort string           `json:"reasoning_effort"`
	RewindTo        *int             `json:"rewind_to"`
}

func (a *app) handleChat(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, detail any) {
		writeDetailError(w, status, detail)
	}

	var req chatRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if req.Message != "" && len(req.Input) > 0 {
		fail(http.StatusBadRequest, "message and input are mutually exclusive")
		return
	}

	cwd := requestCWD(req.CWD)
	settings, err := config.Load(cwd)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}

	resolved, err := config.ResolveProvider(
		settings,
		req.Provider,
		req.Model,
		req.APIKey,
		req.APIBase,
		req.ReasoningEffort,
	)
	if err != nil {
		if errors.Is(err, config.ErrUnsupportedReasoningEffort) {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		fail(http.StatusInternalServerError, err.Error())
		return
	}

	userMessage, err := buildUserMessage(req, cwd)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if err := validateUserMessage(userMessage, resolved); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = "default"
	}

	data, err := a.store.LoadSession(sessionID)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}

	var sessionMeta any
	var baseMessages []message.Message
	if data != nil {
		sessionMeta = data.Session
		baseMessages = append(baseMessages, data.Messages...)
	} else {
		if req.RewindTo != nil {
			fail(http.StatusBadRequest, "rewind_to requires an existing session")
			return
		}
		created, err := a.store.CreateSession(sessionID, cwd)
		if err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		sessionMeta = created.Session
	}

	if req.RewindTo != nil {
		rewindTo := *req.RewindTo
		if rewindTo < 0 || rewindTo >= len(baseMessages) {
			fail(http.StatusBadRequest, fmt.Sprintf("rewind_to must reference a visible message index between 0 and %d", len(baseMessages)-1))
			return
		}
		target := baseMessages[rewindTo]
		if !isRealUserMessage(target) {
			fail(http.StatusBadRequest, "rewind_to must reference a real user message")
			return
		}
		baseMessages = baseMessages[:rewindTo]
		if err := a.store.AppendRewind(sessionID, rewindTo); err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
	}

	agent, err := agentpkg.New(agentpkg.Options{
		Model:              resolved.Model,
		Provider:           resolved.ProviderType,
		CWD:                cwd,
		SessionDir:         a.store.SessionDir(sessionID),
		SessionID:          sessionID,
		APIKey:             resolved.APIKey,
		APIBase:            resolved.APIBase,
		Messages:           baseMessages,
		MaxTokens:          resolved.MaxTokens,
		ContextWindow:      resolved.ContextWindow,
		CompactThreshold:   settings.CompactThreshold,
		ReasoningEffort:    resolved.ReasoningEffort,
		SupportsImageInput: resolved.SupportsImageInput,
		SupportsPDFInput:   resolved.SupportsPDFInput,
	})
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}

	run, err := a.runs.startRun(sessionID, userMessage, baseMessages, agent, func(msg message.Message) error {
		return a.store.AppendMessage(sessionID, msg, cwd)
	})
	if err != nil {
		var activeErr activeRunError
		if errors.As(err, &activeErr) {
			detail := map[string]any{"message": "session already has a running task"}
			if existing := a.runs.getRun(activeErr.RunID); existing != nil {
				detail["run"] = existing.info()
			}
			fail(http.StatusConflict, detail)
			return
		}
		fail(http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run":     run,
		"session": sessionMeta,
	})
}

func (a *app) handleRunStream(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	state := a.runs.getRun(runID)
	if state == nil {
		writeDetailError(w, http.StatusNotFound, "run not found")
		return
	}

	afterValue := strings.TrimSpace(r.URL.Query().Get("after"))
	after := 0
	if afterValue != "" {
		value, err := strconv.Atoi(afterValue)
		if err != nil || value < 0 {
			writeDetailError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = value
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeDetailError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	headers := w.Header()
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastSeq := after
	for {
		pending, finished := state.pendingAfter(lastSeq)
		for _, event := range pending {
			if err := writeSSE(w, event); err != nil {
				return
			}
			lastSeq = eventSeq(event, lastSeq)
			flusher.Flush()
		}

		if finished {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *app) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	run := a.runs.cancelRun(runID)
	if run == nil {
		writeDetailError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"run":    run,
	})
}

func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	cwd := requestCWD(r.URL.Query().Get("cwd"))
	settings, err := config.Load(cwd)
	if err != nil {
		writeDetailError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resolved, err := config.ResolveProvider(settings, "", "", "", "", "")
	if err != nil {
		writeDetailError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	providersInfo := map[string]any{}
	for _, choice := range config.AvailableProviders(settings) {
		spec, ok := provider.LookupSpec(choice.ProviderType)
		if !ok {
			continue
		}
		models := modelsForProvider(settings, choice)
		reasoningModels := make([]string, 0, len(models))
		imageModels := make([]string, 0, len(models))
		pdfModels := make([]string, 0, len(models))

		for _, model := range models {
			resolvedModel, err := config.ResolveProvider(
				settings,
				choice.ProviderName,
				model,
				"",
				choice.APIBase,
				"",
			)
			if err != nil {
				continue
			}
			if resolvedModel.SupportsReasoning {
				reasoningModels = append(reasoningModels, model)
			}
			if resolvedModel.SupportsImageInput {
				imageModels = append(imageModels, model)
			}
			if resolvedModel.SupportsPDFInput {
				pdfModels = append(pdfModels, model)
			}
		}

		info := map[string]any{
			"name":                 choice.ProviderName,
			"provider":             choice.ProviderType,
			"type":                 choice.ProviderType,
			"models":               models,
			"base_url":             choice.APIBase,
			"has_api_key":          true,
			"supports_image_input": len(imageModels) > 0,
			"image_input_models":   imageModels,
			"supports_pdf_input":   len(pdfModels) > 0,
			"pdf_input_models":     pdfModels,
		}

		if spec.SupportsReasoningEffort {
			info["supports_reasoning_effort"] = true
			info["reasoning_models"] = reasoningModels
			info["reasoning_effort"] = responseReasoningEffort(choice.ReasoningEffort)
		}

		providersInfo[choice.ProviderName] = info
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"providers":                providersInfo,
		"default":                  map[string]any{"provider": resolved.ProviderName, "model": resolved.Model},
		"default_reasoning_effort": responseReasoningEffort(settings.DefaultReasoningEffort),
		"reasoning_effort_options": reasoningEffortOptions,
		"cwd":                      cwd,
		"workspace_root":           settings.WorkspaceRoot,
		"config_paths":             settings.ConfigPaths,
	})
}

func buildUserMessage(req chatRequest, cwd string) (message.Message, error) {
	if len(req.Input) == 0 {
		text := strings.TrimSpace(req.Message)
		if text == "" {
			return message.Message{}, fmt.Errorf("message or input is required")
		}
		return message.UserTextMessage(text, nil), nil
	}

	blocks := make([]message.Block, 0, len(req.Input))
	for _, input := range req.Input {
		var block message.Block
		var err error

		switch input.Type {
		case "text":
			if input.IsAttachment {
				name := strings.TrimSpace(input.Name)
				if name == "" {
					name = "attached-file"
				}
				block = message.TextBlock(
					fmt.Sprintf("<file name=\"%s\">\n%s\n</file>", escapeAttachmentAttr(name), input.Text),
					map[string]any{"attachment": true, "path": name},
				)
			} else if input.Text == "" {
				continue
			} else {
				block = message.TextBlock(input.Text, nil)
			}
		case "document":
			block, err = buildDocumentBlock(input, cwd)
		case "image":
			block, err = buildImageBlock(input, cwd)
		default:
			return message.Message{}, fmt.Errorf("unsupported input block type: %s", input.Type)
		}

		if err != nil {
			return message.Message{}, err
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return message.Message{}, fmt.Errorf("input must include at least one non-empty block")
	}
	return message.BuildMessage("user", blocks, nil), nil
}

func buildImageBlock(block chatInputBlock, cwd string) (message.Block, error) {
	if block.Data != "" {
		if strings.TrimSpace(block.MIMEType) == "" {
			return message.Block{}, fmt.Errorf("image data requires mime_type")
		}
		return message.ImageBlock(block.Data, block.MIMEType, defaultString(block.Name, "image"), nil), nil
	}

	resolvedPath, data, err := readInputFile(block, cwd, "image")
	if err != nil {
		return message.Block{}, err
	}

	mimeType := strings.TrimSpace(block.MIMEType)
	if mimeType == "" {
		mimeType = tools.DetectImageMIMEType(resolvedPath)
	}
	if mimeType == "" {
		return message.Block{}, fmt.Errorf("unsupported image file: %s", block.Path)
	}

	return message.ImageBlock(
		base64.StdEncoding.EncodeToString(data),
		mimeType,
		defaultString(block.Name, filepath.Base(resolvedPath)),
		nil,
	), nil
}

func buildDocumentBlock(block chatInputBlock, cwd string) (message.Block, error) {
	if block.Data != "" {
		mimeType := defaultString(block.MIMEType, "application/pdf")
		if mimeType != "application/pdf" {
			return message.Block{}, fmt.Errorf("unsupported document mime_type")
		}
		return message.DocumentBlock(block.Data, mimeType, defaultString(block.Name, "document.pdf"), nil), nil
	}

	resolvedPath, data, err := readInputFile(block, cwd, "document")
	if err != nil {
		return message.Block{}, err
	}

	mimeType := strings.TrimSpace(block.MIMEType)
	if mimeType == "" {
		mimeType = tools.DetectDocumentMIMEType(resolvedPath)
	}
	if mimeType != "application/pdf" {
		return message.Block{}, fmt.Errorf("unsupported document file: %s", block.Path)
	}

	return message.DocumentBlock(
		base64.StdEncoding.EncodeToString(data),
		mimeType,
		defaultString(block.Name, filepath.Base(resolvedPath)),
		nil,
	), nil
}

func readInputFile(block chatInputBlock, cwd, kind string) (string, []byte, error) {
	if strings.TrimSpace(block.Path) == "" {
		return "", nil, fmt.Errorf("%s input requires path or data", kind)
	}

	resolvedPath := tools.ResolvePath(block.Path, cwd)
	info, err := os.Stat(resolvedPath)
	if err != nil || info.IsDir() {
		return "", nil, fmt.Errorf("%s file not found: %s", kind, block.Path)
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read %s file: %w", kind, err)
	}
	return resolvedPath, data, nil
}

func validateUserMessage(userMessage message.Message, resolved config.ResolvedProvider) error {
	for _, block := range userMessage.Content {
		switch block.Type {
		case "image":
			if !resolved.SupportsImageInput {
				return fmt.Errorf("current model does not support image input")
			}
		case "document":
			if !resolved.SupportsPDFInput {
				return fmt.Errorf("current model does not support PDF input")
			}
		}
	}
	return nil
}

func escapeAttachmentAttr(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(value)
}

func isRealUserMessage(msg message.Message) bool {
	if msg.Role != "user" {
		return false
	}
	if msg.Meta["synthetic"] == true {
		return false
	}
	for _, block := range msg.Content {
		if block.Type == "image" || block.Type == "document" {
			return true
		}
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return true
		}
	}
	return false
}

func modelsForProvider(settings config.Settings, resolved config.ResolvedProvider) []string {
	if providerConfig, ok := settings.Providers[resolved.ProviderName]; ok && len(providerConfig.Models) > 0 {
		models := append([]string(nil), providerConfig.ModelOrder...)
		if len(models) == 0 {
			for model := range providerConfig.Models {
				models = append(models, model)
			}
			sort.Strings(models)
		}
		return models
	}
	spec, ok := provider.LookupSpec(resolved.ProviderType)
	if !ok {
		return []string{resolved.Model}
	}
	models := append([]string(nil), spec.DefaultModels...)
	if len(models) == 0 {
		models = []string{resolved.Model}
	}
	return models
}
