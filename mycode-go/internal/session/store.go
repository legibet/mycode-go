package session

import (
	"bufio"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/message"
)

const (
	// MessageFormatVersion is the persisted session format version.
	MessageFormatVersion = 7
	// DefaultSessionTitle is the initial session title.
	DefaultSessionTitle = "New chat"
)

// Meta is the session metadata stored in meta.json.
type Meta struct {
	CWD                  string `json:"cwd"`
	Title                string `json:"title"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
	MessageFormatVersion int    `json:"message_format_version"`
}

// Summary is the session payload returned by the API.
type Summary struct {
	ID string `json:"id"`
	Meta
}

// Data is one loaded session.
type Data struct {
	Session  Summary           `json:"session"`
	Messages []message.Message `json:"messages"`
}

// Store persists append-only sessions.
type Store struct {
	dataDir string
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewStore creates a session store.
func NewStore(dataDir string) *Store {
	if dataDir == "" {
		dataDir = config.ResolveSessionsDir()
	}
	_ = os.MkdirAll(dataDir, 0o755)
	return &Store{
		dataDir: dataDir,
		locks:   map[string]*sync.Mutex{},
	}
}

// BuildRewindEvent returns the persisted rewind event.
func BuildRewindEvent(rewindTo int) message.Message {
	return message.Message{
		Role: "rewind",
		Meta: map[string]any{
			"rewind_to":  rewindTo,
			"created_at": now(),
		},
	}
}

// ApplyRewind truncates visible history according to rewind markers.
func ApplyRewind(messages []message.Message) []message.Message {
	out := make([]message.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "rewind" {
			rewindTo := max(0, asInt(msg.Meta["rewind_to"]))
			if rewindTo < len(out) {
				out = out[:rewindTo]
			} else if rewindTo == 0 {
				out = out[:0]
			}
			continue
		}
		out = append(out, msg)
	}
	return out
}

// DraftSession builds an in-memory draft.
func (s *Store) DraftSession(cwd string) Data {
	nowValue := now()
	meta := Meta{
		CWD:                  absPath(cwd),
		Title:                DefaultSessionTitle,
		CreatedAt:            nowValue,
		UpdatedAt:            nowValue,
		MessageFormatVersion: MessageFormatVersion,
	}
	sessionID := newID()
	return Data{
		Session:  summarize(sessionID, meta),
		Messages: []message.Message{},
	}
}

// CreateSession writes a session immediately.
func (s *Store) CreateSession(sessionID, cwd string) (Data, error) {
	data := s.DraftSession(cwd)
	if sessionID != "" {
		data.Session.ID = sessionID
	}
	lock := s.sessionLock(data.Session.ID)
	lock.Lock()
	defer lock.Unlock()

	if err := s.ensureSessionDir(data.Session.ID); err != nil {
		return Data{}, err
	}
	if err := s.writeMeta(data.Session.ID, data.Session.Meta); err != nil {
		return Data{}, err
	}
	file, err := os.OpenFile(s.messagesPath(data.Session.ID), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return Data{}, err
	}
	_ = file.Close()
	return data, nil
}

// ListSessions returns sessions sorted by updated_at descending.
func (s *Store) ListSessions(cwd string) ([]Summary, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	filterCWD := absPath(cwd)
	out := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := s.readMeta(entry.Name())
		if err != nil {
			continue
		}
		if filterCWD != "" && absPath(meta.CWD) != filterCWD {
			continue
		}
		out = append(out, summarize(entry.Name(), meta))
	}
	slices.SortFunc(out, func(a, b Summary) int {
		return cmp.Compare(b.UpdatedAt, a.UpdatedAt)
	})
	return out, nil
}

// LatestSession returns the latest session for a workspace.
func (s *Store) LatestSession(cwd string) (*Summary, error) {
	sessions, err := s.ListSessions(cwd)
	if err != nil || len(sessions) == 0 {
		return nil, err
	}
	return &sessions[0], nil
}

// LoadSession loads one session and applies replay rules.
func (s *Store) LoadSession(sessionID string) (*Data, error) {
	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	meta, err := s.readMeta(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	rawMessages, err := s.readMessages(sessionID)
	if err != nil {
		return nil, err
	}
	// Visible state = raw JSONL minus rewound tails. `compact` markers stay
	// inline; the agent substitutes them when calling the provider.
	visible := ApplyRewind(rawMessages)
	if err := s.repairInterruptedToolLoop(sessionID, &meta, &visible); err != nil {
		return nil, err
	}

	return &Data{Session: summarize(sessionID, meta), Messages: visible}, nil
}

// DeleteSession removes a session directory.
func (s *Store) DeleteSession(sessionID string) error {
	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	return os.RemoveAll(s.sessionDir(sessionID))
}

// ClearSession removes persisted messages and keeps meta.
func (s *Store) ClearSession(sessionID string) error {
	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	meta, err := s.readMeta(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	meta.Title = DefaultSessionTitle
	meta.UpdatedAt = now()
	if err := s.writeMeta(sessionID, meta); err != nil {
		return err
	}
	return os.WriteFile(s.messagesPath(sessionID), nil, 0o644)
}

// AppendRewind appends a rewind event.
func (s *Store) AppendRewind(sessionID string, rewindTo int) error {
	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	meta, err := s.readMeta(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := s.ensureSessionDir(sessionID); err != nil {
		return err
	}
	if err := appendJSONL(s.messagesPath(sessionID), BuildRewindEvent(rewindTo)); err != nil {
		return err
	}
	meta.UpdatedAt = now()
	return s.writeMeta(sessionID, meta)
}

// AppendMessage appends one canonical message.
func (s *Store) AppendMessage(sessionID string, msg message.Message, cwd string) error {
	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	if err := s.ensureSessionDir(sessionID); err != nil {
		return err
	}

	meta, err := s.readMeta(sessionID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		nowValue := now()
		meta = Meta{
			CWD:                  absPath(cwd),
			Title:                DefaultSessionTitle,
			CreatedAt:            nowValue,
			UpdatedAt:            nowValue,
			MessageFormatVersion: MessageFormatVersion,
		}
	}

	if err := appendJSONL(s.messagesPath(sessionID), msg); err != nil {
		return err
	}

	meta.UpdatedAt = now()
	if meta.Title == DefaultSessionTitle && msg.Role == "user" {
		if title := strings.TrimSpace(strings.ReplaceAll(message.FlattenText(msg, false), "\n", " ")); title != "" {
			titleRunes := []rune(title)
			if len(titleRunes) > 48 {
				title = string(titleRunes[:48])
			}
			meta.Title = title
		}
	}
	return s.writeMeta(sessionID, meta)
}

// SessionDir returns the absolute session directory.
func (s *Store) SessionDir(sessionID string) string {
	return s.sessionDir(sessionID)
}

func (s *Store) repairInterruptedToolLoop(sessionID string, meta *Meta, messages *[]message.Message) error {
	visible := *messages
	pendingIDs := make([]string, 0)
	pendingIndex := -1
	for i := len(visible) - 1; i >= 0; i-- {
		msg := visible[i]
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "tool_use" && block.ID != "" {
				pendingIDs = append(pendingIDs, block.ID)
			}
		}
		if len(pendingIDs) > 0 {
			pendingIndex = i
			break
		}
	}

	if pendingIndex == -1 {
		return nil
	}

	completedIDs := map[string]struct{}{}
	for _, msg := range visible[pendingIndex+1:] {
		if msg.Role != "user" {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.ToolUseID != "" {
				completedIDs[block.ToolUseID] = struct{}{}
			}
		}
	}

	missing := make([]string, 0, len(pendingIDs))
	for _, id := range pendingIDs {
		if _, ok := completedIDs[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	repairBlocks := make([]message.Block, 0, len(missing))
	for _, id := range missing {
		repairBlocks = append(repairBlocks, message.ToolResultBlock(
			id,
			"error: tool call was interrupted",
			nil,
			true,
			nil,
			nil,
		))
	}
	repair := message.BuildMessage("user", repairBlocks, nil)
	if err := appendJSONL(s.messagesPath(sessionID), repair); err != nil {
		return err
	}

	meta.UpdatedAt = now()
	if err := s.writeMeta(sessionID, *meta); err != nil {
		return err
	}

	*messages = append(*messages, repair)
	return nil
}

func (s *Store) readMessages(sessionID string) ([]message.Message, error) {
	file, err := os.Open(s.messagesPath(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	out := make([]message.Message, 0)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg message.Message
		if err := json.Unmarshal(line, &msg); err == nil {
			out = append(out, msg)
		}
	}
	return out, scanner.Err()
}

func (s *Store) readMeta(sessionID string) (Meta, error) {
	data, err := os.ReadFile(s.metaPath(sessionID))
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, os.ErrNotExist
	}
	return meta, nil
}

func (s *Store) writeMeta(sessionID string, meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath(sessionID), data, 0o644)
}

func (s *Store) ensureSessionDir(sessionID string) error {
	if err := os.MkdirAll(s.sessionDir(sessionID), 0o755); err != nil {
		return err
	}
	return nil
}

func (s *Store) sessionDir(sessionID string) string {
	return filepath.Join(s.dataDir, sessionID)
}

func (s *Store) metaPath(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), "meta.json")
}

func (s *Store) messagesPath(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), "messages.jsonl")
}

func (s *Store) sessionLock(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lock, ok := s.locks[sessionID]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	s.locks[sessionID] = lock
	return lock
}

func appendJSONL(path string, msg message.Message) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func absPath(path string) string {
	if path == "" {
		return ""
	}
	value, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return value
}

func summarize(sessionID string, meta Meta) Summary {
	return Summary{
		ID:   sessionID,
		Meta: meta,
	}
}

func asInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

func newID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
