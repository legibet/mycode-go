package core

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agentpkg "github.com/legibet/mycode-go/internal/agent"
	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/internal/provider"
	"github.com/legibet/mycode-go/internal/session"
	"github.com/legibet/mycode-go/internal/tools"
	"github.com/legibet/mycode-go/internal/workspace"
)

var ReasoningEffortOptions = []string{"auto", "none", "low", "medium", "high", "xhigh"}

type Service struct {
	store *session.Store
	runs  *RunManager
}

type Options struct {
	Store *session.Store
	Runs  *RunManager
	Sink  EventSink
}

type StatusError struct {
	Status int
	Detail any
}

func (e *StatusError) Error() string {
	if text, ok := e.Detail.(string); ok {
		return text
	}
	return fmt.Sprintf("request failed with status %d", e.Status)
}

func statusError(status int, detail any) *StatusError {
	return &StatusError{Status: status, Detail: detail}
}

type ChatInputBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	Path         string `json:"path"`
	Data         string `json:"data"`
	MIMEType     string `json:"mime_type"`
	Name         string `json:"name"`
	IsAttachment bool   `json:"is_attachment"`
}

type ChatRequest struct {
	SessionID       string           `json:"session_id"`
	Message         string           `json:"message"`
	Input           []ChatInputBlock `json:"input"`
	Provider        string           `json:"provider"`
	Model           string           `json:"model"`
	CWD             string           `json:"cwd"`
	APIKey          string           `json:"api_key"`
	APIBase         string           `json:"api_base"`
	ReasoningEffort string           `json:"reasoning_effort"`
	RewindTo        *int             `json:"rewind_to"`
}

type ChatResponse struct {
	Run     map[string]any `json:"run"`
	Session any            `json:"session"`
}

type SessionCreateRequest struct {
	CWD string `json:"cwd"`
}

type SessionResponse struct {
	Session       any               `json:"session"`
	Messages      []message.Message `json:"messages"`
	ActiveRun     any               `json:"active_run"`
	PendingEvents []map[string]any  `json:"pending_events"`
}

type SessionListItem struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	CWD                  string `json:"cwd"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
	MessageFormatVersion int    `json:"message_format_version"`
	IsRunning            bool   `json:"is_running"`
}

type SessionsResponse struct {
	Sessions []SessionListItem `json:"sessions"`
}

type CancelRunResponse struct {
	Status string         `json:"status"`
	Run    map[string]any `json:"run"`
}

type DecideRequest struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
}

type DecideResponse struct {
	Status string `json:"status"`
}

func NewService(opts Options) *Service {
	store := opts.Store
	if store == nil {
		store = session.NewStore("")
	}
	runs := opts.Runs
	if runs == nil {
		runs = NewRunManager(opts.Sink)
	}
	return &Service{store: store, runs: runs}
}

func (s *Service) StartChat(req ChatRequest) (ChatResponse, error) {
	if req.Message != "" && len(req.Input) > 0 {
		return ChatResponse{}, statusError(http.StatusBadRequest, "message and input are mutually exclusive")
	}

	cwd := RequestCWD(req.CWD)
	settings, err := config.Load(cwd)
	if err != nil {
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
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
			return ChatResponse{}, statusError(http.StatusBadRequest, err.Error())
		}
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	userMessage, err := buildUserMessage(req, cwd)
	if err != nil {
		return ChatResponse{}, statusError(http.StatusBadRequest, err.Error())
	}
	if err := validateUserMessage(userMessage, resolved); err != nil {
		return ChatResponse{}, statusError(http.StatusBadRequest, err.Error())
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = "default"
	}

	data, err := s.store.LoadSession(sessionID)
	if err != nil {
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	var sessionMeta any
	var baseMessages []message.Message
	if data != nil {
		sessionMeta = data.Session
		baseMessages = append(baseMessages, data.Messages...)
	} else {
		if req.RewindTo != nil {
			return ChatResponse{}, statusError(http.StatusBadRequest, "rewind_to requires an existing session")
		}
		created, err := s.store.CreateSession(sessionID, cwd)
		if err != nil {
			return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
		}
		sessionMeta = created.Session
	}

	if req.RewindTo != nil {
		rewindTo := *req.RewindTo
		if rewindTo < 0 || rewindTo >= len(baseMessages) {
			return ChatResponse{}, statusError(http.StatusBadRequest, fmt.Sprintf("rewind_to must reference a visible message index between 0 and %d", len(baseMessages)-1))
		}
		target := baseMessages[rewindTo]
		if !isRealUserMessage(target) {
			return ChatResponse{}, statusError(http.StatusBadRequest, "rewind_to must reference a real user message")
		}
		baseMessages = baseMessages[:rewindTo]
		if err := s.store.AppendRewind(sessionID, rewindTo); err != nil {
			return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
		}
	}

	agent, err := agentpkg.New(agentpkg.Options{
		Model:              resolved.Model,
		Provider:           resolved.ProviderType,
		CWD:                cwd,
		SessionDir:         s.store.SessionDir(sessionID),
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
		Permission:         settings.Permission,
		PermissionReviewer: func(ctx context.Context, req permissions.ReviewRequest) permissions.ReviewDecision {
			return s.runs.requestDecision(ctx, sessionID, req)
		},
		SkillRoots: permissions.SkillRoots(cwd, config.ResolveHome()),
	})
	if err != nil {
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	run, err := s.runs.startRun(sessionID, userMessage, baseMessages, agent, func(msg message.Message) error {
		return s.store.AppendMessage(sessionID, msg, cwd)
	})
	if err != nil {
		var activeErr ActiveRunError
		if errors.As(err, &activeErr) {
			detail := map[string]any{"message": "session already has a running task"}
			if existing := s.runs.getRun(activeErr.RunID); existing != nil {
				detail["run"] = existing.info()
			}
			return ChatResponse{}, statusError(http.StatusConflict, detail)
		}
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	return ChatResponse{Run: run, Session: sessionMeta}, nil
}

func (s *Service) RunEventsAfter(runID string, after int) ([]map[string]any, bool, error) {
	pending, finished, ok := s.runs.pendingAfter(runID, after)
	if !ok {
		return nil, false, statusError(http.StatusNotFound, "run not found")
	}
	return pending, finished, nil
}

func (s *Service) CancelRun(runID string) (CancelRunResponse, error) {
	run := s.runs.cancelRun(runID)
	if run == nil {
		return CancelRunResponse{}, statusError(http.StatusNotFound, "run not found")
	}
	return CancelRunResponse{Status: "ok", Run: run}, nil
}

func (s *Service) DecideRun(runID string, req DecideRequest) (DecideResponse, error) {
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return DecideResponse{}, statusError(http.StatusBadRequest, "request_id is required")
	}

	var decision permissions.ReviewDecision
	switch req.Decision {
	case string(permissions.ReviewAllow):
		decision = permissions.ReviewAllow
	case string(permissions.ReviewDeny):
		decision = permissions.ReviewDeny
	default:
		return DecideResponse{}, statusError(http.StatusBadRequest, "decision must be allow or deny")
	}

	if !s.runs.resolveDecision(runID, requestID, decision) {
		return DecideResponse{}, statusError(http.StatusNotFound, "permission request not found")
	}
	return DecideResponse{Status: "ok"}, nil
}

func (s *Service) Config(cwd string) (map[string]any, error) {
	cwd = RequestCWD(cwd)
	settings, err := config.Load(cwd)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	resolved, err := config.ResolveProvider(settings, "", "", "", "", "")
	if err != nil {
		return nil, statusError(http.StatusServiceUnavailable, err.Error())
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
			info["reasoning_effort"] = ResponseReasoningEffort(choice.ReasoningEffort)
		}

		providersInfo[choice.ProviderName] = info
	}

	return map[string]any{
		"providers":                providersInfo,
		"default":                  map[string]any{"provider": resolved.ProviderName, "model": resolved.Model},
		"default_reasoning_effort": ResponseReasoningEffort(settings.DefaultReasoningEffort),
		"reasoning_effort_options": ReasoningEffortOptions,
		"cwd":                      cwd,
		"config_paths":             settings.ConfigPaths,
	}, nil
}

func (s *Service) CreateSession(req SessionCreateRequest) (session.Data, error) {
	data, err := s.store.CreateSession("", RequestCWD(req.CWD))
	if err != nil {
		return session.Data{}, statusError(http.StatusInternalServerError, err.Error())
	}
	return data, nil
}

func (s *Service) ListSessions(cwd string) (SessionsResponse, error) {
	items, err := s.store.ListSessions(strings.TrimSpace(cwd))
	if err != nil {
		return SessionsResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	sessions := make([]SessionListItem, 0, len(items))
	for _, item := range items {
		sessions = append(sessions, SessionListItem{
			ID:                   item.ID,
			Title:                item.Title,
			CWD:                  item.CWD,
			CreatedAt:            item.CreatedAt,
			UpdatedAt:            item.UpdatedAt,
			MessageFormatVersion: item.MessageFormatVersion,
			IsRunning:            s.runs.hasActiveRun(item.ID),
		})
	}
	return SessionsResponse{Sessions: sessions}, nil
}

func (s *Service) LoadSession(sessionID string) (SessionResponse, error) {
	data, err := s.store.LoadSession(sessionID)
	if err != nil {
		return SessionResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	resp := SessionResponse{
		Messages:      []message.Message{},
		PendingEvents: []map[string]any{},
	}
	if data != nil {
		resp.Session = data.Session
		resp.Messages = data.Messages
	}

	if active := s.runs.snapshotSession(sessionID); active != nil {
		resp.ActiveRun = active.Run
		resp.Messages = active.Messages
		resp.PendingEvents = active.PendingEvents
	}

	return resp, nil
}

func (s *Service) DeleteSession(sessionID string) error {
	if s.runs.hasActiveRun(sessionID) {
		return statusError(http.StatusConflict, "session has a running task")
	}
	if err := s.store.DeleteSession(sessionID); err != nil {
		return statusError(http.StatusInternalServerError, err.Error())
	}
	return nil
}

func (s *Service) ClearSession(sessionID string) error {
	if s.runs.hasActiveRun(sessionID) {
		return statusError(http.StatusConflict, "session has a running task")
	}
	if err := s.store.ClearSession(sessionID); err != nil {
		return statusError(http.StatusInternalServerError, err.Error())
	}
	return nil
}

func (s *Service) WorkspaceRoots() map[string]any {
	return map[string]any{"roots": workspace.Roots()}
}

func (s *Service) WorkspaceBrowse(root, path string) (workspace.BrowseResult, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return workspace.BrowseResult{}, statusError(http.StatusBadRequest, "root is required")
	}
	return workspace.Browse(root, path), nil
}

func RequestCWD(value string) string {
	resolved := strings.TrimSpace(value)
	if resolved == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "."
		}
		resolved = cwd
	}
	if absolute, err := filepath.Abs(resolved); err == nil {
		return absolute
	}
	return resolved
}

func ResponseReasoningEffort(value string) string {
	if strings.TrimSpace(value) == "" {
		return "auto"
	}
	return value
}

func buildUserMessage(req ChatRequest, cwd string) (message.Message, error) {
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

func buildImageBlock(block ChatInputBlock, cwd string) (message.Block, error) {
	if block.Data != "" {
		if strings.TrimSpace(block.MIMEType) == "" {
			return message.Block{}, fmt.Errorf("image data requires mime_type")
		}
		return message.ImageBlock(block.Data, block.MIMEType, cmp.Or(block.Name, "image"), nil), nil
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
		cmp.Or(block.Name, filepath.Base(resolvedPath)),
		nil,
	), nil
}

func buildDocumentBlock(block ChatInputBlock, cwd string) (message.Block, error) {
	if block.Data != "" {
		mimeType := cmp.Or(block.MIMEType, "application/pdf")
		if mimeType != "application/pdf" {
			return message.Block{}, fmt.Errorf("unsupported document mime_type")
		}
		return message.DocumentBlock(block.Data, mimeType, cmp.Or(block.Name, "document.pdf"), nil), nil
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
		cmp.Or(block.Name, filepath.Base(resolvedPath)),
		nil,
	), nil
}

func readInputFile(block ChatInputBlock, cwd, kind string) (string, []byte, error) {
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
			slices.Sort(models)
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
