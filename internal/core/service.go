package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	agentpkg "github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/attachment"
	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/internal/prompt"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/provider"
	"github.com/legibet/mycode-go/session"
	"github.com/legibet/mycode-go/tools"
)

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
	ID        string `json:"id"`
	Title     string `json:"title"`
	CWD       string `json:"cwd"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	IsRunning bool   `json:"is_running"`
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

func NewService(opts Options) (*Service, error) {
	store := opts.Store
	if store == nil {
		var err error
		store, err = session.NewStore(config.ResolveSessionsDir())
		if err != nil {
			return nil, err
		}
	}
	runs := opts.Runs
	if runs == nil {
		runs = NewRunManager(opts.Sink)
	}
	return &Service{store: store, runs: runs}, nil
}

func (s *Service) StartChat(req ChatRequest) (ChatResponse, error) {
	if err := validateChatRequestShape(req); err != nil {
		return ChatResponse{}, err
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
	if err := message.ValidateMediaSupport(userMessage, resolved.SupportsImageInput, resolved.SupportsPDFInput); err != nil {
		return ChatResponse{}, statusError(http.StatusBadRequest, err.Error())
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = "default"
	}

	unlockSession := s.runs.lockSession(sessionID)
	defer unlockSession()

	if active := s.runs.activeRunInfo(sessionID); active != nil {
		return ChatResponse{}, activeRunConflict(active)
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

	agent, err := agentpkg.New(agentpkg.Config{
		Model:              resolved.Model,
		Provider:           resolved.ProviderType,
		CWD:                cwd,
		SessionDir:         s.store.SessionDir(sessionID),
		SessionID:          sessionID,
		APIKey:             resolved.APIKey,
		APIBase:            resolved.APIBase,
		System:             prompt.Build(cwd, settings.Project, config.ResolveHome()),
		Messages:           baseMessages,
		MaxTokens:          resolved.MaxTokens,
		ContextWindow:      resolved.ContextWindow,
		CompactThreshold:   settings.CompactThreshold,
		DisableCompact:     settings.CompactThreshold <= 0,
		ReasoningEffort:    resolved.ReasoningEffort,
		SupportsImageInput: resolved.SupportsImageInput,
		SupportsPDFInput:   resolved.SupportsPDFInput,
		ToolSpecs:          tools.DefaultSpecs(),
		Hooks: tools.Hooks{
			BeforeTool: []tools.BeforeToolHook{
				permissions.ToolHook(
					settings.Permission,
					func(ctx context.Context, req permissions.ReviewRequest) permissions.ReviewDecision {
						return s.runs.requestDecision(ctx, sessionID, req)
					},
					cwd,
					settings.Project,
					permissions.SkillRoots(cwd, settings.Project, config.ResolveHome()),
				),
			},
		},
	})
	if err != nil {
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	run, err := s.runs.startRun(sessionID, userMessage, baseMessages, agent, func(msg message.Message) error {
		return s.store.AppendMessage(sessionID, msg, cwd)
	})
	if err != nil {
		if activeErr, ok := errors.AsType[ActiveRunError](err); ok {
			if existing := s.runs.getRun(activeErr.RunID); existing != nil {
				return ChatResponse{}, activeRunConflict(existing.info())
			}
			return ChatResponse{}, activeRunConflict(nil)
		}
		return ChatResponse{}, statusError(http.StatusInternalServerError, err.Error())
	}

	return ChatResponse{Run: run, Session: sessionMeta}, nil
}

func (s *Service) RunEventsAfter(runID string, after int) (RunEventBatch, error) {
	batch, ok := s.runs.pendingAfter(runID, after)
	if !ok {
		return RunEventBatch{}, statusError(http.StatusNotFound, "run not found")
	}
	return batch, nil
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
	var setupError map[string]string
	if err != nil {
		setupError = map[string]string{"message": err.Error()}
	}

	providersInfo := map[string]any{}
	for _, choice := range config.AvailableProviders(settings) {
		spec, ok := provider.LookupSpec(choice.ProviderType)
		if !ok {
			continue
		}
		models := config.ModelsForProvider(settings, choice)
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
			info["reasoning_effort"] = config.ResponseReasoningEffort(choice.ReasoningEffort)
		}

		providersInfo[choice.ProviderName] = info
	}

	defaultPayload := map[string]string{"provider": "", "model": ""}
	if err == nil {
		defaultPayload = map[string]string{"provider": resolved.ProviderName, "model": resolved.Model}
	}

	return map[string]any{
		"providers":                providersInfo,
		"default":                  defaultPayload,
		"default_reasoning_effort": config.ResponseReasoningEffort(settings.DefaultReasoningEffort),
		"reasoning_effort_options": config.ReasoningEffortOptions,
		"cwd":                      cwd,
		"cwd_exists":               isExistingDir(cwd),
		"project":                  settings.Project,
		"config_paths":             settings.ConfigPaths,
		"setup_error":              setupError,
	}, nil
}

func (s *Service) Settings() (map[string]any, error) {
	path := filepath.Join(config.ResolveHome(), "config.json")
	raw, order, exists, err := config.ReadRawSettings(path)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	return config.BuildSettingsResponse(path, exists, raw, order), nil
}

func (s *Service) UpdateSettings(req SettingsRequest) (map[string]any, error) {
	path := filepath.Join(config.ResolveHome(), "config.json")
	existing, _, _, err := config.ReadRawSettings(path)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	incoming := map[string]any{}
	order := config.Order{}
	if text := strings.TrimSpace(string(req.Config)); text != "" && text != "null" {
		if err := json.Unmarshal(req.Config, &incoming); err != nil {
			return nil, statusError(http.StatusBadRequest, err.Error())
		}
		order = config.ParseConfigOrder(req.Config)
	}
	config.MergeAPIKeys(incoming, existing)
	cleaned, err := config.ValidateGlobalConfig(incoming)
	if err != nil {
		return nil, statusError(http.StatusBadRequest, err.Error())
	}
	if err := config.WriteSettingsFile(path, cleaned, order); err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	raw, savedOrder, exists, err := config.ReadRawSettings(path)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, err.Error())
	}
	return config.BuildSettingsResponse(path, exists, raw, savedOrder), nil
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
			ID:        item.ID,
			Title:     item.Title,
			CWD:       item.CWD,
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
			IsRunning: s.runs.hasActiveRun(item.ID),
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
	unlockSession := s.runs.lockSession(sessionID)
	defer unlockSession()

	if s.runs.hasActiveRun(sessionID) {
		return statusError(http.StatusConflict, "session has a running task")
	}
	if err := s.store.DeleteSession(sessionID); err != nil {
		return statusError(http.StatusInternalServerError, err.Error())
	}
	return nil
}

func (s *Service) ClearSession(sessionID string) error {
	unlockSession := s.runs.lockSession(sessionID)
	defer unlockSession()

	if s.runs.hasActiveRun(sessionID) {
		return statusError(http.StatusConflict, "session has a running task")
	}
	if err := s.store.ClearSession(sessionID); err != nil {
		return statusError(http.StatusInternalServerError, err.Error())
	}
	return nil
}

func activeRunConflict(run map[string]any) error {
	detail := map[string]any{"message": "session already has a running task"}
	if run != nil {
		detail["run"] = run
	}
	return statusError(http.StatusConflict, detail)
}

func (s *Service) WorkspaceRoots() map[string]any {
	return map[string]any{"roots": workspaceRoots()}
}

func (s *Service) WorkspaceBrowse(root, path string) (BrowseResult, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return BrowseResult{}, statusError(http.StatusBadRequest, "root is required")
	}
	return browseWorkspace(root, path), nil
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

func validateChatRequestShape(req ChatRequest) error {
	hasMessage := strings.TrimSpace(req.Message) != ""
	hasInput := len(req.Input) > 0
	if hasMessage && hasInput {
		return statusError(http.StatusUnprocessableEntity, "message and input are mutually exclusive")
	}
	if !hasMessage && !hasInput {
		return statusError(http.StatusUnprocessableEntity, "message or input is required")
	}

	for _, block := range req.Input {
		switch block.Type {
		case "text":
			continue
		case "image":
			if strings.TrimSpace(block.Path) == "" && block.Data == "" {
				return statusError(http.StatusUnprocessableEntity, "image input requires path or data")
			}
			if block.Data != "" && strings.TrimSpace(block.MIMEType) == "" {
				return statusError(http.StatusUnprocessableEntity, "image data requires mime_type")
			}
		case "document":
			if strings.TrimSpace(block.Path) == "" && block.Data == "" {
				return statusError(http.StatusUnprocessableEntity, "document input requires path or data")
			}
			mimeType := strings.TrimSpace(block.MIMEType)
			if mimeType != "" && mimeType != "application/pdf" {
				return statusError(http.StatusUnprocessableEntity, "unsupported document mime_type")
			}
		default:
			return statusError(http.StatusUnprocessableEntity, fmt.Sprintf("unsupported input block type: %s", block.Type))
		}
	}
	return nil
}

func buildUserMessage(req ChatRequest, cwd string) (message.Message, error) {
	if len(req.Input) == 0 {
		text := strings.TrimSpace(req.Message)
		if text == "" {
			return message.Message{}, errors.New("message or input is required")
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
				block, err = buildAttachmentBlock(attachment.Text(input.Text, name), cwd)
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
		return message.Message{}, errors.New("input must include at least one non-empty block")
	}
	return message.BuildMessage("user", blocks, nil), nil
}

func buildImageBlock(block ChatInputBlock, cwd string) (message.Block, error) {
	if block.Data != "" {
		if strings.TrimSpace(block.MIMEType) == "" {
			return message.Block{}, errors.New("image data requires mime_type")
		}
		return message.ImageBlock(block.Data, block.MIMEType, cmp.Or(block.Name, "image"), nil), nil
	}

	mediaBlock, err := buildAttachmentBlock(attachment.PathWithName(block.Path, block.Name), cwd)
	if err != nil {
		return message.Block{}, err
	}
	if mediaBlock.Type != "image" {
		return message.Block{}, fmt.Errorf("unsupported image file: %s", block.Path)
	}
	return mediaBlock, nil
}

func buildDocumentBlock(block ChatInputBlock, cwd string) (message.Block, error) {
	if block.Data != "" {
		mimeType := cmp.Or(block.MIMEType, "application/pdf")
		if mimeType != "application/pdf" {
			return message.Block{}, errors.New("unsupported document mime_type")
		}
		return message.DocumentBlock(block.Data, mimeType, cmp.Or(block.Name, "document.pdf"), nil), nil
	}

	mediaBlock, err := buildAttachmentBlock(attachment.PathWithName(block.Path, block.Name), cwd)
	if err != nil {
		return message.Block{}, err
	}
	if mediaBlock.Type != "document" || mediaBlock.MIMEType != "application/pdf" {
		return message.Block{}, fmt.Errorf("unsupported document file: %s", block.Path)
	}
	return mediaBlock, nil
}

func buildAttachmentBlock(item attachment.Attachment, cwd string) (message.Block, error) {
	blocks, err := attachment.Build([]attachment.Attachment{item}, attachment.Options{CWD: cwd})
	if err != nil {
		return message.Block{}, err
	}
	if len(blocks) == 0 {
		return message.Block{}, errors.New("attachment produced no blocks")
	}
	return blocks[0], nil
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
