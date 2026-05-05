package core

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

type SettingsRequest struct {
	Config json.RawMessage `json:"config"`
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
	if !isExistingDir(cwd) {
		return ChatResponse{}, statusError(http.StatusBadRequest, fmt.Sprintf("Working directory does not exist: %s", cwd))
	}
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
	)
	if err != nil {
		if errors.Is(err, config.ErrUnsupportedReasoningEffort) {
			return ChatResponse{}, statusError(http.StatusBadRequest, err.Error())
		}
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		effort, err := config.NormalizeReasoningEffort(req.ReasoningEffort)
		if err != nil {
			return ChatResponse{}, statusError(http.StatusBadRequest, err.Error())
		}
		// Per-request overrides honor the same gate as configured defaults:
		// only send reasoning_effort when the model supports reasoning and the
		// adapter exposes the effort knob.
		if effort != "" && resolved.SupportsReasoning && resolved.SupportsEffortToggle {
			resolved.ReasoningEffort = effort
		}
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
		Project:            settings.Project,
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
		SkillRoots: permissions.SkillRoots(cwd, settings.Project, config.ResolveHome()),
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
	resolved, err := config.ResolveProvider(settings, "", "", "", "")
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
		"cwd_exists":               isExistingDir(cwd),
		"project":                  settings.Project,
		"config_paths":             settings.ConfigPaths,
	}, nil
}

func (s *Service) Settings() (map[string]any, error) {
	path := filepath.Join(config.ResolveHome(), "config.json")
	raw, order, exists, err := readRawConfig(path)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	return settingsResponse(path, exists, raw, order), nil
}

func (s *Service) UpdateSettings(req SettingsRequest) (map[string]any, error) {
	path := filepath.Join(config.ResolveHome(), "config.json")
	existing, _, _, err := readRawConfig(path)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	incoming := map[string]any{}
	order := config.ConfigOrder{}
	if text := strings.TrimSpace(string(req.Config)); text != "" && text != "null" {
		if err := json.Unmarshal(req.Config, &incoming); err != nil {
			return nil, statusError(http.StatusBadRequest, err.Error())
		}
		order = config.ParseConfigOrder(req.Config)
	}
	mergeExistingAPIKeys(incoming, existing)
	cleaned, err := config.ValidateGlobalConfig(incoming)
	if err != nil {
		return nil, statusError(http.StatusBadRequest, err.Error())
	}
	if err := writeJSONFileAtomic(path, cleaned, order); err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	raw, savedOrder, exists, err := readRawConfig(path)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	return settingsResponse(path, exists, raw, savedOrder), nil
}

func (s *Service) CreateSession(req SessionCreateRequest) (session.Data, error) {
	cwd := RequestCWD(req.CWD)
	if !isExistingDir(cwd) {
		return session.Data{}, statusError(http.StatusBadRequest, fmt.Sprintf("Working directory does not exist: %s", cwd))
	}
	data, err := s.store.CreateSession("", cwd)
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
		resp.Messages = redactDocumentData(data.Messages)
	}

	if active := s.runs.snapshotSession(sessionID); active != nil {
		resp.ActiveRun = active.Run
		resp.Messages = redactDocumentData(active.Messages)
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

func isExistingDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// redactDocumentData strips the base64 payload from `document` blocks so the
// API does not leak large PDF binaries on every reload.
func redactDocumentData(messages []message.Message) []message.Message {
	out := make([]message.Message, 0, len(messages))
	for _, msg := range messages {
		hasDocument := false
		for _, block := range msg.Content {
			if block.Type == "document" && block.Data != "" {
				hasDocument = true
				break
			}
		}
		if !hasDocument {
			out = append(out, msg)
			continue
		}
		clone := message.Clone(msg)
		for i := range clone.Content {
			if clone.Content[i].Type == "document" {
				clone.Content[i].Data = ""
			}
		}
		out = append(out, clone)
	}
	return out
}

func readRawConfig(path string) (map[string]any, config.ConfigOrder, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, config.ConfigOrder{}, false, nil
	}
	if err != nil {
		return nil, config.ConfigOrder{}, false, err
	}
	if !info.Mode().IsRegular() {
		return map[string]any{}, config.ConfigOrder{}, false, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, config.ConfigOrder{}, false, err
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, config.ConfigOrder{}, true, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	raw, ok := parsed.(map[string]any)
	if !ok {
		return nil, config.ConfigOrder{}, true, fmt.Errorf("%s must contain a JSON object", path)
	}
	return raw, config.ParseConfigOrder(data), true, nil
}

func settingsResponse(path string, exists bool, raw map[string]any, order config.ConfigOrder) map[string]any {
	providerTypes := make([]string, 0, len(provider.Specs()))
	envNames := map[string]struct{}{}
	typeEnvVars := map[string][]string{}
	typeDefaultModels := map[string][]string{}
	for _, spec := range provider.Specs() {
		providerTypes = append(providerTypes, spec.ID)
		if len(spec.EnvAPIKeyNames) > 0 {
			typeEnvVars[spec.ID] = append([]string(nil), spec.EnvAPIKeyNames...)
		}
		if len(spec.DefaultModels) > 0 {
			typeDefaultModels[spec.ID] = append([]string(nil), spec.DefaultModels...)
		}
		if spec.AutoDiscoverable {
			for _, name := range spec.EnvAPIKeyNames {
				envNames[name] = struct{}{}
			}
		}
	}
	slices.Sort(providerTypes)
	providers, _ := raw["providers"].(map[string]any)
	for _, rawEntry := range providers {
		rawEntry, _ := rawEntry.(map[string]any)
		if rawEntry == nil {
			continue
		}
		if apiKey, _ := rawEntry["api_key"].(string); apiKey != "" {
			if ref := config.IsAPIKeyEnvRef(apiKey); ref != "" {
				envNames[ref] = struct{}{}
			}
		}
	}
	env := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(envNames)) {
		env[name] = strings.TrimSpace(os.Getenv(name)) != ""
	}

	return map[string]any{
		"path":   path,
		"exists": exists,
		"config": presentConfig(raw, order),
		"options": map[string]any{
			"provider_types":    providerTypes,
			"permission_levels": config.PermissionLevelOptions(),
			"permission_modes":  config.PermissionModeOptions(),
			"reasoning_efforts": ReasoningEffortOptions,
		},
		"env":                          env,
		"provider_type_env_vars":       typeEnvVars,
		"provider_type_default_models": typeDefaultModels,
	}
}

func presentConfig(raw map[string]any, order config.ConfigOrder) map[string]any {
	out := maps.Clone(raw)
	if out == nil {
		out = map[string]any{}
	}
	providers, _ := raw["providers"].(map[string]any)
	if len(providers) == 0 {
		return out
	}
	presentedProviders := map[string]any{}
	for name, rawEntry := range providers {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			presentedProviders[name] = rawEntry
			continue
		}
		entry = maps.Clone(entry)
		apiKey, _ := entry["api_key"].(string)
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			entry["api_key"] = nil
			entry["api_key_saved"] = false
		} else if config.IsAPIKeyEnvRef(apiKey) != "" {
			entry["api_key_saved"] = false
		} else {
			entry["api_key"] = nil
			entry["api_key_saved"] = true
		}

		models, _ := entry["models"].(map[string]any)
		if len(models) > 0 {
			keys := orderedKeys(models, order.ModelOrder[name])
			entry["models"] = keys
			overrides := map[string]any{}
			for _, key := range keys {
				modelOverride, _ := models[key].(map[string]any)
				if len(modelOverride) > 0 {
					overrides[key] = modelOverride
				}
			}
			if len(overrides) > 0 {
				entry["model_overrides"] = overrides
			}
		}
		presentedProviders[name] = entry
	}
	out["providers"] = orderedMap{values: presentedProviders, order: order.ProviderOrder}
	return out
}

func mergeExistingAPIKeys(incoming, existing map[string]any) {
	incomingProviders, _ := incoming["providers"].(map[string]any)
	existingProviders, _ := existing["providers"].(map[string]any)
	for name, rawEntry := range incomingProviders {
		entry, _ := rawEntry.(map[string]any)
		if entry == nil || entry["api_key"] != nil {
			continue
		}
		prior, _ := existingProviders[name].(map[string]any)
		if prior != nil {
			if apiKey, ok := prior["api_key"]; ok {
				entry["api_key"] = apiKey
				continue
			}
		}
		delete(entry, "api_key")
	}
}

func writeJSONFileAtomic(path string, payload map[string]any, order config.ConfigOrder) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".")
	if err != nil {
		return err
	}
	tmpName := file.Name()
	defer func() { _ = os.Remove(tmpName) }()

	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	providers, _ := payload["providers"].(map[string]any)
	if len(providers) > 0 {
		orderedProviders := map[string]any{}
		for _, name := range orderedKeys(providers, order.ProviderOrder) {
			rawEntry := providers[name]
			entry, ok := rawEntry.(map[string]any)
			if ok {
				entry = maps.Clone(entry)
				if models, _ := entry["models"].(map[string]any); len(models) > 0 {
					entry["models"] = orderedMap{values: models, order: order.ModelOrder[name]}
				}
				rawEntry = entry
			}
			orderedProviders[name] = rawEntry
		}
		payload = maps.Clone(payload)
		payload["providers"] = orderedMap{values: orderedProviders, order: order.ProviderOrder}
	}
	if err := encoder.Encode(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

type orderedMap struct {
	values map[string]any
	order  []string
}

// orderedMap preserves provider/model order in settings JSON.
func (m orderedMap) MarshalJSON() ([]byte, error) {
	var buf strings.Builder
	buf.WriteByte('{')
	for i, key := range orderedKeys(m.values, m.order) {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(m.values[key])
		if err != nil {
			return nil, err
		}
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return []byte(buf.String()), nil
}

func orderedKeys(values map[string]any, preferred []string) []string {
	keys := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, key := range preferred {
		if _, ok := values[key]; !ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if _, ok := seen[key]; ok {
			continue
		}
		keys = append(keys, key)
	}
	return keys
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
