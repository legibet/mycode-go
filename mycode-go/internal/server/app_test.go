package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legibet/mycode-go/internal/core"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/provider"
	"github.com/legibet/mycode-go/internal/session"
)

func TestWorkspaceRoutes(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "demo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MYCODE_WORKSPACE_ROOTS", root)
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))

	t.Run("roots", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/workspaces/roots", nil)
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}

		var payload struct {
			Roots []string `json:"roots"`
		}
		decodeBody(t, recorder, &payload)
		if len(payload.Roots) != 1 || payload.Roots[0] != root {
			t.Fatalf("unexpected roots: %#v", payload.Roots)
		}
	})

	t.Run("browse", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/workspaces/browse?root="+root+"&path=projects",
			nil,
		)
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}

		var payload struct {
			Path    string `json:"path"`
			Current string `json:"current"`
			Entries []struct {
				Name string `json:"name"`
				Path string `json:"path"`
			} `json:"entries"`
			Error string `json:"error"`
		}
		decodeBody(t, recorder, &payload)
		if payload.Error != "" {
			t.Fatalf("unexpected error: %s", payload.Error)
		}
		if payload.Path != "projects" {
			t.Fatalf("unexpected path: %s", payload.Path)
		}
		if !strings.HasSuffix(payload.Current, filepath.Join("projects")) {
			t.Fatalf("unexpected current path: %s", payload.Current)
		}
		if len(payload.Entries) != 1 || payload.Entries[0].Name != "demo" || payload.Entries[0].Path != "projects/demo" {
			t.Fatalf("unexpected entries: %#v", payload.Entries)
		}
	})
}

func TestServeStaticFromConfiguredWebRoot(t *testing.T) {
	webRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "app.js"), []byte("console.log('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newApp(true, webRoot, session.NewStore(t.TempDir()), core.NewRunManager(nil))

	t.Run("asset", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/app.js", nil)
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}
		if body := recorder.Body.String(); body != "console.log('ok')" {
			t.Fatalf("unexpected asset body: %q", body)
		}
	})

	t.Run("spa fallback", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/chat/demo", nil)
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}
		if body := recorder.Body.String(); body != "index" {
			t.Fatalf("unexpected index body: %q", body)
		}
	})
}

func TestSessionLifecycle(t *testing.T) {
	store := session.NewStore(t.TempDir())
	handler := newApp(false, "", store, core.NewRunManager(nil))
	cwd := t.TempDir()

	recorder := performJSON(
		t,
		handler,
		http.MethodPost,
		"/api/sessions",
		map[string]any{
			"cwd": cwd,
		},
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var created struct {
		Session struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			CWD   string `json:"cwd"`
		} `json:"session"`
	}
	decodeBody(t, recorder, &created)
	if created.Session.ID == "" {
		t.Fatal("expected session id")
	}
	if created.Session.Title != session.DefaultSessionTitle {
		t.Fatalf("unexpected title: %s", created.Session.Title)
	}
	if created.Session.CWD != cwd {
		t.Fatalf("unexpected cwd: %s", created.Session.CWD)
	}

	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/sessions?cwd="+cwd, nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	var listed struct {
		Sessions []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			IsRunning bool   `json:"is_running"`
		} `json:"sessions"`
	}
	decodeBody(t, recorder, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != created.Session.ID {
		t.Fatalf("unexpected sessions: %#v", listed.Sessions)
	}
	if listed.Sessions[0].IsRunning {
		t.Fatal("session should not be running")
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/sessions/"+created.Session.ID, nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	var loaded struct {
		Session any `json:"session"`
	}
	decodeBody(t, recorder, &loaded)
	if loaded.Session == nil {
		t.Fatal("expected session payload")
	}
}

func TestChatRejectsMutuallyExclusiveMessageAndInput(t *testing.T) {
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))

	recorder := performJSON(
		t,
		handler,
		http.MethodPost,
		"/api/chat",
		map[string]any{
			"provider": "openai",
			"message":  "hi",
			"input": []map[string]any{
				{"type": "text", "text": "hello"},
			},
		},
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}

	var payload struct {
		Detail string `json:"detail"`
	}
	decodeBody(t, recorder, &payload)
	if payload.Detail != "message and input are mutually exclusive" {
		t.Fatalf("unexpected detail: %s", payload.Detail)
	}
}

func TestChatRejectsRewindForNewSessionWithoutCreatingFiles(t *testing.T) {
	isolateServerConfigTest(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	store := session.NewStore(t.TempDir())
	handler := newApp(false, "", store, core.NewRunManager(nil))

	recorder := performJSON(
		t,
		handler,
		http.MethodPost,
		"/api/chat",
		map[string]any{
			"session_id": "missing",
			"message":    "retry",
			"rewind_to":  0,
			"provider":   "openai",
			"model":      "gpt-5.4",
			"cwd":        t.TempDir(),
		},
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Detail string `json:"detail"`
	}
	decodeBody(t, recorder, &payload)
	if payload.Detail != "rewind_to requires an existing session" {
		t.Fatalf("unexpected detail: %s", payload.Detail)
	}

	if sessions, err := store.ListSessions(""); err != nil || len(sessions) != 0 {
		t.Fatalf("unexpected sessions after rejected rewind: %#v err=%v", sessions, err)
	}
}

func TestChatRejectsUnsupportedReasoningEffortAsBadRequest(t *testing.T) {
	isolateServerConfigTest(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))
	recorder := performJSON(
		t,
		handler,
		http.MethodPost,
		"/api/chat",
		map[string]any{
			"session_id":       "bad-effort",
			"message":          "hello",
			"provider":         "openai",
			"model":            "gpt-5.4",
			"cwd":              t.TempDir(),
			"reasoning_effort": "maximum",
		},
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestChatRejectsRewindToCompactSummary(t *testing.T) {
	isolateServerConfigTest(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	store := session.NewStore(t.TempDir())
	handler := newApp(false, "", store, core.NewRunManager(nil))
	created, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := created.Session.ID

	compactMarker := message.BuildMessage("compact", []message.Block{message.TextBlock("summary of hello+hi", nil)}, map[string]any{"provider": "p", "model": "m"})
	for _, msg := range []message.Message{
		message.UserTextMessage("hello", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("hi", nil)}, "openai", "gpt-5.4", "", "", 0, nil),
		compactMarker,
		message.UserTextMessage("explain X", nil),
	} {
		if err := store.AppendMessage(sessionID, msg, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}

	// Targeting the inline compact marker (index 2) is invalid: rewind_to
	// must reference a real user prompt.
	recorder := performJSON(
		t,
		handler,
		http.MethodPost,
		"/api/chat",
		map[string]any{
			"session_id": sessionID,
			"message":    "retry",
			"rewind_to":  2,
			"provider":   "openai",
			"model":      "gpt-5.4",
			"cwd":        "/tmp",
		},
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Detail string `json:"detail"`
	}
	decodeBody(t, recorder, &payload)
	if payload.Detail != "rewind_to must reference a real user message" {
		t.Fatalf("unexpected detail: %s", payload.Detail)
	}
}

func TestConfigRoute(t *testing.T) {
	isolateServerConfigTest(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/config?cwd="+t.TempDir(), nil)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Providers map[string]map[string]any `json:"providers"`
		Default   map[string]any            `json:"default"`
	}
	decodeBody(t, recorder, &payload)
	if len(payload.Providers) == 0 {
		t.Fatal("expected at least one provider")
	}
	if _, ok := payload.Providers["openai"]; !ok {
		t.Fatalf("expected openai provider, got %#v", payload.Providers)
	}
	if payload.Default["provider"] == "" {
		t.Fatalf("expected default provider, got %#v", payload.Default)
	}
}

func TestSettingsRouteMasksSecretsAndReportsEnv(t *testing.T) {
	isolateServerConfigTest(t)
	home := filepath.Join(t.TempDir(), "home", ".mycode")
	t.Setenv("MYCODE_HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "set")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	writeServerJSON(t, filepath.Join(home, "config.json"), map[string]any{
		"providers": map[string]any{
			"anthropic": map[string]any{"type": "anthropic", "api_key": "sk-secret"},
			"router":    map[string]any{"type": "openrouter", "api_key": "${MY_CUSTOM_KEY}"},
		},
	})

	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload map[string]any
	decodeBody(t, recorder, &payload)
	providers := payload["config"].(map[string]any)["providers"].(map[string]any)
	anthropic := providers["anthropic"].(map[string]any)
	router := providers["router"].(map[string]any)
	env := payload["env"].(map[string]any)
	if anthropic["api_key"] != nil || anthropic["api_key_saved"] != true {
		t.Fatalf("unexpected anthropic config: %#v", anthropic)
	}
	if router["api_key"] != "${MY_CUSTOM_KEY}" || router["api_key_saved"] != false {
		t.Fatalf("unexpected router config: %#v", router)
	}
	if env["ANTHROPIC_API_KEY"] != true || env["MY_CUSTOM_KEY"] != false {
		t.Fatalf("unexpected env: %#v", env)
	}
}

func TestSettingsPutPreservesOrClearsExistingAPIKey(t *testing.T) {
	isolateServerConfigTest(t)
	home := filepath.Join(t.TempDir(), "home", ".mycode")
	t.Setenv("MYCODE_HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")

	cases := []struct {
		name     string
		apiKey   any
		expected any
	}{
		{name: "keep", apiKey: nil, expected: "sk-old"},
		{name: "clear", apiKey: "", expected: nil},
		{name: "replace", apiKey: "sk-new", expected: "sk-new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeServerJSON(t, path, map[string]any{
				"providers": map[string]any{
					"anthropic": map[string]any{"type": "anthropic", "api_key": "sk-old"},
				},
			})
			handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))
			recorder := performJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
				"config": map[string]any{
					"providers": map[string]any{
						"anthropic": map[string]any{"type": "anthropic", "api_key": tc.apiKey},
					},
				},
			})
			if recorder.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
			}
			onDisk := map[string]any{}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &onDisk); err != nil {
				t.Fatal(err)
			}
			providerConfig := onDisk["providers"].(map[string]any)["anthropic"].(map[string]any)
			if providerConfig["api_key"] != tc.expected {
				t.Fatalf("unexpected on-disk config: %#v", providerConfig)
			}
		})
	}
}

func TestSettingsPutRejectsInvalidProviderType(t *testing.T) {
	isolateServerConfigTest(t)
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))
	recorder := performJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"config": map[string]any{
			"providers": map[string]any{
				"weird": map[string]any{"type": "not-a-real-provider"},
			},
		},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func performJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func writeServerJSON(t *testing.T, path string, payload any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), dst); err != nil {
		t.Fatalf("failed to decode response: %v; body=%s", err, recorder.Body.String())
	}
}

func isolateServerConfigTest(t *testing.T) {
	t.Helper()
	t.Setenv("MYCODE_HOME", filepath.Join(t.TempDir(), "home", ".mycode"))
	t.Setenv("PORT", "")
	for _, spec := range provider.Specs() {
		for _, envName := range spec.EnvAPIKeyNames {
			t.Setenv(envName, "")
		}
	}
}
