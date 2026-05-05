package session

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/util"
)

const (
	// MessageFormatVersion is the persisted session format version. Bumping
	// it is a Python-side compatibility decision.
	MessageFormatVersion = 7
	DefaultSessionTitle  = "New chat"
)

// Meta is the JSON saved in meta.json.
type Meta struct {
	CWD                  string `json:"cwd"`
	Title                string `json:"title"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
	MessageFormatVersion int    `json:"message_format_version"`
}

type Summary struct {
	ID string `json:"id"`
	Meta
}

type Data struct {
	Session  Summary           `json:"session"`
	Messages []message.Message `json:"messages"`
}

// Store keeps a per-session mutex so concurrent operations on the same
// session can't interleave, while different sessions remain concurrent.
type Store struct {
	dataDir string
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
}

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

// BuildRewindEvent is the marker appended on rewind requests; ApplyRewind
// truncates visible history from this marker.
func BuildRewindEvent(rewindTo int) message.Message {
	return message.Message{
		Role: "rewind",
		Meta: map[string]any{
			"rewind_to":  rewindTo,
			"created_at": now(),
		},
	}
}

// ApplyRewind collapses raw JSONL into visible history. Rewind markers are dropped.
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

// DraftSession returns an in-memory session that has not been written to disk.
func (s *Store) DraftSession(cwd string) Data {
	nowValue := now()
	meta := Meta{
		CWD:                  util.ExpandAbs(cwd),
		Title:                DefaultSessionTitle,
		CreatedAt:            nowValue,
		UpdatedAt:            nowValue,
		MessageFormatVersion: MessageFormatVersion,
	}
	return Data{
		Session:  Summary{ID: util.RandomHex16(), Meta: meta},
		Messages: []message.Message{},
	}
}

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
	// Touch messages.jsonl so readers always see both files.
	file, err := os.OpenFile(s.messagesPath(data.Session.ID), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return Data{}, err
	}
	_ = file.Close()
	return data, nil
}

// ListSessions returns sessions (filtered by cwd when non-empty), sorted by updated_at desc.
func (s *Store) ListSessions(cwd string) ([]Summary, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	filterCWD := util.ExpandAbs(cwd)
	out := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := s.readMeta(entry.Name())
		if err != nil {
			continue
		}
		if filterCWD != "" && util.ExpandAbs(meta.CWD) != filterCWD {
			continue
		}
		out = append(out, Summary{ID: entry.Name(), Meta: meta})
	}
	slices.SortFunc(out, func(a, b Summary) int {
		return cmp.Compare(b.UpdatedAt, a.UpdatedAt)
	})
	return out, nil
}

func (s *Store) LatestSession(cwd string) (*Summary, error) {
	sessions, err := s.ListSessions(cwd)
	if err != nil || len(sessions) == 0 {
		return nil, err
	}
	return &sessions[0], nil
}

// LoadSession returns the visible (post-rewind) history. `compact` markers
// stay inline; the agent substitutes them when calling the provider. Orphan
// tool_use blocks are closed by the provider adapter at replay time.
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
	return &Data{Session: Summary{ID: sessionID, Meta: meta}, Messages: ApplyRewind(rawMessages)}, nil
}

func (s *Store) DeleteSession(sessionID string) error {
	lock := s.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	return os.RemoveAll(s.SessionDir(sessionID))
}

// ClearSession resets messages but keeps meta so the session id stays addressable.
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

// AppendMessage appends one message and refreshes meta. The first non-empty
// user turn becomes the session title (truncated to 48 runes).
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
			CWD:                  util.ExpandAbs(cwd),
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

func (s *Store) readMessages(sessionID string) ([]message.Message, error) {
	file, err := os.Open(s.messagesPath(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	// Large tool_result outputs occasionally exceed bufio's default 64KB buffer.
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
		// Treat a corrupt meta.json as missing so the next write recreates it.
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
	return os.MkdirAll(s.SessionDir(sessionID), 0o755)
}

func (s *Store) SessionDir(sessionID string) string {
	return filepath.Join(s.dataDir, sessionID)
}

func (s *Store) metaPath(sessionID string) string {
	return filepath.Join(s.SessionDir(sessionID), "meta.json")
}

func (s *Store) messagesPath(sessionID string) string {
	return filepath.Join(s.SessionDir(sessionID), "messages.jsonl")
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
	defer func() { _ = file.Close() }()

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	return err
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
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
