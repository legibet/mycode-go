package session

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/legibet/mycode-go/message"
)

const (
	DefaultSessionTitle = "New chat"
)

// Meta is the JSON saved in meta.json.
type Meta struct {
	CWD       string `json:"cwd"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
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
	indexMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewStore(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("session data dir is required")
	}
	dataDir = absPath(dataDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create session data dir: %w", err)
	}
	return &Store{
		dataDir: dataDir,
		locks:   map[string]*sync.Mutex{},
	}, nil
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
		CWD:       absPath(cwd),
		Title:     DefaultSessionTitle,
		CreatedAt: nowValue,
		UpdatedAt: nowValue,
	}
	return Data{
		Session:  Summary{ID: randomHex16(), Meta: meta},
		Messages: []message.Message{},
	}
}

func (s *Store) SessionExists(sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	_, err := os.Stat(s.metaPath(sessionID))
	return err == nil
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
	file, err := os.OpenFile(s.MessagesPath(data.Session.ID), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return Data{}, err
	}
	if err := file.Close(); err != nil {
		return Data{}, err
	}
	return data, nil
}

// ListSessions returns sessions (filtered by cwd when non-empty), sorted by updated_at desc.
func (s *Store) ListSessions(cwd string) ([]Summary, error) {
	s.indexMu.Lock()
	index, err := s.readIndexLocked()
	s.indexMu.Unlock()
	if err != nil {
		return nil, err
	}

	filterCWD := absPath(cwd)
	out := make([]Summary, 0, len(index))
	for sessionID, meta := range index {
		if filterCWD != "" && absPath(meta.CWD) != filterCWD {
			continue
		}
		out = append(out, Summary{ID: sessionID, Meta: meta})
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

	if err := os.RemoveAll(s.SessionDir(sessionID)); err != nil {
		return err
	}

	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	index, err := s.readIndexLocked()
	if err != nil {
		return err
	}
	delete(index, sessionID)
	return s.writeIndexLocked(index)
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
	return os.WriteFile(s.MessagesPath(sessionID), nil, 0o644)
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
	if err := appendJSONL(s.MessagesPath(sessionID), BuildRewindEvent(rewindTo)); err != nil {
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
			CWD:       absPath(cwd),
			Title:     DefaultSessionTitle,
			CreatedAt: nowValue,
			UpdatedAt: nowValue,
		}
	}

	if err := appendJSONL(s.MessagesPath(sessionID), msg); err != nil {
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
	file, err := os.Open(s.MessagesPath(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReader(file)
	out := make([]message.Message, 0)
	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return out, err
			}
			continue
		}
		var msg message.Message
		if err := json.Unmarshal(line, &msg); err == nil {
			out = append(out, msg)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
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
	if err := os.WriteFile(s.metaPath(sessionID), data, 0o644); err != nil {
		return err
	}

	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	index, err := s.readIndexLocked()
	if err != nil {
		return err
	}
	index[sessionID] = meta
	return s.writeIndexLocked(index)
}

func (s *Store) readIndexLocked() (map[string]Meta, error) {
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.rebuildIndexLocked()
		}
		return nil, err
	}
	var index map[string]Meta
	if err := json.Unmarshal(data, &index); err != nil || index == nil {
		return s.rebuildIndexLocked()
	}
	return index, nil
}

func (s *Store) rebuildIndexLocked() (map[string]Meta, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]Meta{}, nil
		}
		return nil, err
	}

	index := make(map[string]Meta)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := s.readMeta(entry.Name())
		if err == nil {
			index[entry.Name()] = meta
		}
	}
	return index, s.writeIndexLocked(index)
}

func (s *Store) writeIndexLocked(index map[string]Meta) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath(), data, 0o644)
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

func (s *Store) indexPath() string {
	return filepath.Join(s.dataDir, "index.json")
}

// MessagesPath returns the JSONL transcript path for a session.
func (s *Store) MessagesPath(sessionID string) string {
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
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func randomHex16() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
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

func absPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}
