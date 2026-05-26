package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/legibet/mycode-go/internal/core"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/provider"
	"github.com/legibet/mycode-go/internal/session"
)

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

func TestDefaultAppDoesNotEnableCORS(t *testing.T) {
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "/api/settings", nil)
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set("Access-Control-Request-Method", "GET")
	handler.ServeHTTP(recorder, request)

	if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected CORS header: %s", recorder.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestDevAppAllowsOnlyLocalViteCORS(t *testing.T) {
	handler := NewDevHandler()

	allowed := httptest.NewRecorder()
	allowedReq := httptest.NewRequest(http.MethodOptions, "/api/settings", nil)
	allowedReq.Header.Set("Origin", "http://localhost:5173")
	allowedReq.Header.Set("Access-Control-Request-Method", "GET")
	handler.ServeHTTP(allowed, allowedReq)

	if allowed.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("unexpected allowed CORS header: %s", allowed.Header().Get("Access-Control-Allow-Origin"))
	}

	denied := httptest.NewRecorder()
	deniedReq := httptest.NewRequest(http.MethodOptions, "/api/settings", nil)
	deniedReq.Header.Set("Origin", "https://example.com")
	deniedReq.Header.Set("Access-Control-Request-Method", "GET")
	handler.ServeHTTP(denied, deniedReq)

	if denied.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected denied CORS header: %s", denied.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestChatRequestShapeValidation(t *testing.T) {
	cases := []map[string]any{
		{"message": "hi", "input": []map[string]any{{"type": "text", "text": "also hi"}}},
		{"message": "   "},
		{"input": []map[string]any{{"type": "image", "data": "abc"}}},
		{"input": []map[string]any{{"type": "document", "data": "abc", "mime_type": "text/plain"}}},
		{"input": []map[string]any{{"type": "image"}}},
	}
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))

	for _, payload := range cases {
		recorder := performJSON(t, handler, http.MethodPost, "/api/chat", payload)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 for %#v, got %d: %s", payload, recorder.Code, recorder.Body.String())
		}
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

func TestChatCapabilityFailureDoesNotCreateSession(t *testing.T) {
	isolateServerConfigTest(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	home := filepath.Join(t.TempDir(), "home", ".mycode")
	t.Setenv("MYCODE_HOME", home)
	writeServerJSON(t, filepath.Join(home, "config.json"), map[string]any{
		"default": map[string]any{"provider": "openai", "model": "gpt-5.4"},
		"providers": map[string]any{
			"openai": map[string]any{
				"models": map[string]any{
					"gpt-5.4": map[string]any{"supports_image_input": false},
				},
			},
		},
	})

	store := session.NewStore(t.TempDir())
	handler := newApp(false, "", store, core.NewRunManager(nil))

	recorder := performJSON(
		t,
		handler,
		http.MethodPost,
		"/api/chat",
		map[string]any{
			"session_id": "new-session",
			"cwd":        t.TempDir(),
			"provider":   "openai",
			"model":      "gpt-5.4",
			"input": []map[string]any{{
				"type":      "image",
				"data":      "abc",
				"mime_type": "image/png",
			}},
		},
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Detail string `json:"detail"`
	}
	decodeBody(t, recorder, &payload)
	if payload.Detail != "current model does not support image input" {
		t.Fatalf("unexpected detail: %s", payload.Detail)
	}
	if loaded, err := store.LoadSession("new-session"); err != nil || loaded != nil {
		t.Fatalf("unexpected session after rejected chat: %#v err=%v", loaded, err)
	}
}

func TestChatTextAttachmentIsPersistedAsFileBlock(t *testing.T) {
	isolateServerConfigTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		defer func() { _ = r.Body.Close() }()
		_, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.output_text.delta\n"+
				"data: {\"type\":\"response.output_text.delta\",\"content_index\":0,\"delta\":\"ok\",\"item_id\":\"msg_1\",\"logprobs\":[],\"output_index\":0,\"sequence_number\":1}\n\n"+
				"event: response.output_item.done\n"+
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"annotations\":[],\"logprobs\":[],\"text\":\"ok\"}],\"role\":\"assistant\"},\"output_index\":0,\"sequence_number\":2}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.4\",\"object\":\"response\",\"output\":[],\"status\":\"completed\"}}\n\n",
		)
	}))
	defer upstream.Close()

	home := filepath.Join(t.TempDir(), "home", ".mycode")
	t.Setenv("MYCODE_HOME", home)
	writeServerJSON(t, filepath.Join(home, "config.json"), map[string]any{
		"default": map[string]any{"provider": "openai", "model": "gpt-5.4"},
		"providers": map[string]any{
			"openai": map[string]any{
				"api_key":  "sk-test",
				"api_base": upstream.URL,
			},
		},
	})

	store := session.NewStore(t.TempDir())
	handler := newApp(false, "", store, core.NewRunManager(nil))
	recorder := performJSON(
		t,
		handler,
		http.MethodPost,
		"/api/chat",
		map[string]any{
			"session_id": "attachments",
			"cwd":        t.TempDir(),
			"input": []map[string]any{{
				"type":          "text",
				"text":          "print(1)",
				"name":          `report <"draft">.py`,
				"is_attachment": true,
			}},
		},
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		Run map[string]any `json:"run"`
	}
	decodeBody(t, recorder, &response)
	runID, _ := response.Run["id"].(string)
	if runID == "" {
		t.Fatalf("missing run id: %#v", response)
	}

	stream := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID+"/stream", nil)
	handler.ServeHTTP(stream, request)
	if stream.Code != http.StatusOK {
		t.Fatalf("expected stream 200, got %d: %s", stream.Code, stream.Body.String())
	}

	loaded, err := store.LoadSession("attachments")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || len(loaded.Messages) < 1 || len(loaded.Messages[0].Content) != 1 {
		t.Fatalf("unexpected loaded session: %#v", loaded)
	}
	got := loaded.Messages[0].Content[0].Text
	want := "<file name=\"report &lt;&quot;draft&quot;&gt;.py\">\nprint(1)\n</file>"
	if got != want {
		t.Fatalf("unexpected attachment block: %q", got)
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

func TestSettingsRouteReturnsEmptyWhenNoFile(t *testing.T) {
	isolateServerConfigTest(t)
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload map[string]any
	decodeBody(t, recorder, &payload)
	if payload["exists"] != false {
		t.Fatalf("unexpected exists: %#v", payload["exists"])
	}
	if config, ok := payload["config"].(map[string]any); !ok || len(config) != 0 {
		t.Fatalf("unexpected config: %#v", payload["config"])
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

func TestSettingsPutWritesModelsAsDict(t *testing.T) {
	isolateServerConfigTest(t)
	home := filepath.Join(t.TempDir(), "home", ".mycode")
	t.Setenv("MYCODE_HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))

	recorder := performJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"config": map[string]any{
			"default": map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6"},
			"providers": map[string]any{
				"anthropic": map[string]any{
					"type":    "anthropic",
					"api_key": "sk",
					"models":  []string{"claude-sonnet-4-6"},
				},
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
	models := onDisk["providers"].(map[string]any)["anthropic"].(map[string]any)["models"].(map[string]any)
	if len(models) != 1 {
		t.Fatalf("unexpected models: %#v", models)
	}
	override, ok := models["claude-sonnet-4-6"].(map[string]any)
	if !ok || len(override) != 0 {
		t.Fatalf("unexpected model override: %#v", models["claude-sonnet-4-6"])
	}
}

func TestSettingsPutDropsEmptyFields(t *testing.T) {
	isolateServerConfigTest(t)
	home := filepath.Join(t.TempDir(), "home", ".mycode")
	t.Setenv("MYCODE_HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))

	recorder := performJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"config": map[string]any{
			"default":    map[string]any{"provider": "", "model": nil},
			"permission": nil,
			"providers": map[string]any{
				"anthropic": map[string]any{"api_key": "", "base_url": nil, "models": []any{}},
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
	if !reflect.DeepEqual(onDisk, map[string]any{"providers": map[string]any{"anthropic": map[string]any{}}}) {
		t.Fatalf("unexpected on-disk config: %#v", onDisk)
	}
}

func TestSettingsPutPersistsCompactThresholdFalse(t *testing.T) {
	isolateServerConfigTest(t)
	home := filepath.Join(t.TempDir(), "home", ".mycode")
	t.Setenv("MYCODE_HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	handler := newApp(false, "", session.NewStore(t.TempDir()), core.NewRunManager(nil))

	recorder := performJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"config": map[string]any{
			"default": map[string]any{"compact_threshold": false},
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
	if !reflect.DeepEqual(onDisk, map[string]any{"default": map[string]any{"compact_threshold": false}}) {
		t.Fatalf("unexpected on-disk config: %#v", onDisk)
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
