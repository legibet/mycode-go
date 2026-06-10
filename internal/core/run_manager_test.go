package core

import (
	"context"
	"iter"
	"testing"
	"time"

	agentpkg "github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/message"
)

// fakeRunAgent drives RunManager without a real provider: it emits `before`
// events, optionally blocks until `release` (or ctx cancel), then emits `after`.
type fakeRunAgent struct {
	before  []agentpkg.Event
	release chan struct{}
	after   []agentpkg.Event
}

func (f *fakeRunAgent) ChatMessage(ctx context.Context, _ message.Message) iter.Seq[agentpkg.Event] {
	return func(yield func(agentpkg.Event) bool) {
		for _, event := range f.before {
			if !yield(event) {
				return
			}
		}
		if f.release != nil {
			select {
			case <-ctx.Done():
				return
			case <-f.release:
			}
		}
		for _, event := range f.after {
			if !yield(event) {
				return
			}
		}
	}
}

func (f *fakeRunAgent) Cancel() {}

func textEvent(delta string) agentpkg.Event {
	return agentpkg.Event{Type: "text", Data: map[string]any{"delta": delta}}
}

func blockingAgent() *fakeRunAgent {
	return &fakeRunAgent{before: []agentpkg.Event{textEvent("reply")}, release: make(chan struct{})}
}

func completeAgent() *fakeRunAgent {
	return &fakeRunAgent{before: []agentpkg.Event{textEvent("reply")}}
}

func errorAgent() *fakeRunAgent {
	return &fakeRunAgent{before: []agentpkg.Event{
		textEvent("partial"),
		{Type: "error", Data: map[string]any{"message": "upstream failed"}},
	}}
}

func waitFor(t *testing.T, deadline time.Duration, fn func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func runInfo(t *testing.T, manager *RunManager, runID string) map[string]any {
	t.Helper()
	state := manager.getRun(runID)
	if state == nil {
		t.Fatalf("run %q not found", runID)
	}
	return state.info()
}

func waitRunStatus(t *testing.T, manager *RunManager, runID, status string) map[string]any {
	t.Helper()
	var info map[string]any
	waitFor(t, time.Second, func() bool {
		info = runInfo(t, manager, runID)
		return info["status"] == status
	})
	return info
}

func waitRunEvent(t *testing.T, manager *RunManager, runID, eventType string) RunEvent {
	t.Helper()
	var matched RunEvent
	waitFor(t, time.Second, func() bool {
		batch, ok := manager.pendingAfter(runID, 0)
		if !ok {
			return false
		}
		for _, event := range batch.Events {
			if event.Type == eventType {
				matched = event
				return true
			}
		}
		return false
	})
	return matched
}

func TestRunManagerSnapshotIncludesUserMessageAndPendingEvents(t *testing.T) {
	manager := NewRunManager(nil)
	agent := blockingAgent()
	userMessage := message.UserTextMessage("build feature", nil)
	baseMessages := []message.Message{
		message.AssistantMessage([]message.Block{message.TextBlock("Earlier", nil)}, "openai", "gpt-5.4", "", "", 0, nil),
	}

	run, err := manager.startRun("session-1", userMessage, baseMessages, agent)
	if err != nil {
		t.Fatal(err)
	}

	var snapshot *runSnapshot
	waitFor(t, time.Second, func() bool {
		snapshot = manager.snapshotSession("session-1")
		return snapshot != nil && len(snapshot.PendingEvents) > 0
	})

	if snapshot.Run["id"] != run["id"] {
		t.Fatalf("unexpected run info: %#v", snapshot.Run)
	}
	if len(snapshot.Messages) != 2 || snapshot.Messages[0].Content[0].Text != "Earlier" || snapshot.Messages[1].Content[0].Text != "build feature" {
		t.Fatalf("unexpected snapshot messages: %#v", snapshot.Messages)
	}
	if len(snapshot.PendingEvents) != 1 || snapshot.PendingEvents[0]["type"] != "text" || snapshot.PendingEvents[0]["delta"] != "reply" {
		t.Fatalf("unexpected pending events: %#v", snapshot.PendingEvents)
	}

	close(agent.release)
	waitRunStatus(t, manager, run["id"].(string), "completed")
}

func TestRunManagerSameSessionCannotStartSecondRun(t *testing.T) {
	manager := NewRunManager(nil)
	first := blockingAgent()
	userMessage := message.UserTextMessage("first", nil)

	run, err := manager.startRun("session-1", userMessage, nil, first)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.startRun("session-1", message.UserTextMessage("second", nil), nil, completeAgent()); err == nil {
		t.Fatal("expected ActiveRunError")
	}

	close(first.release)
	waitRunStatus(t, manager, run["id"].(string), "completed")
}

func TestRunManagerCancelOnlyMarksTargetRunCancelled(t *testing.T) {
	manager := NewRunManager(nil)
	first := blockingAgent()
	second := blockingAgent()

	firstRun, err := manager.startRun("session-1", message.UserTextMessage("first", nil), nil, first)
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := manager.startRun("session-2", message.UserTextMessage("second", nil), nil, second)
	if err != nil {
		t.Fatal(err)
	}

	cancelled := manager.cancelRun(firstRun["id"].(string))
	if cancelled == nil {
		t.Fatal("expected cancelled run info")
	}
	if cancelled["status"] != "cancelled" {
		t.Fatalf("unexpected cancelled run: %#v", cancelled)
	}
	if manager.hasActiveRun("session-1") {
		t.Fatal("expected session-1 to have no active run")
	}

	updatedFirst := runInfo(t, manager, firstRun["id"].(string))
	updatedSecond := runInfo(t, manager, secondRun["id"].(string))
	if updatedFirst["status"] != "cancelled" {
		t.Fatalf("unexpected first run: %#v", updatedFirst)
	}
	if updatedSecond["status"] != "running" {
		t.Fatalf("unexpected second run: %#v", updatedSecond)
	}

	close(second.release)
	waitRunStatus(t, manager, secondRun["id"].(string), "completed")
}

func TestRunManagerFinishedRunStaysAvailableForReconnectWindow(t *testing.T) {
	manager := NewRunManager(nil)

	run, err := manager.startRun("session-1", message.UserTextMessage("done", nil), nil, completeAgent())
	if err != nil {
		t.Fatal(err)
	}

	waitRunStatus(t, manager, run["id"].(string), "completed")

	finished := runInfo(t, manager, run["id"].(string))
	if finished["status"] != "completed" {
		t.Fatalf("unexpected finished run: %#v", finished)
	}
	if snapshot := manager.snapshotSession("session-1"); snapshot != nil {
		t.Fatalf("expected no active snapshot: %#v", snapshot)
	}
}

func TestRunManagerEventsRespectAfterAndFinish(t *testing.T) {
	manager := NewRunManager(nil)

	run, err := manager.startRun("session-1", message.UserTextMessage("done", nil), nil, completeAgent())
	if err != nil {
		t.Fatal(err)
	}
	runID := run["id"].(string)
	waitRunStatus(t, manager, runID, "completed")

	batch, ok := manager.pendingAfter(runID, 0)
	if !ok {
		t.Fatal("expected run events")
	}
	if !batch.Finished || len(batch.Events) != 1 || batch.Events[0].Seq != 1 || batch.Events[0].Type != "text" {
		t.Fatalf("unexpected event replay: %#v", batch)
	}

	afterFirst, ok := manager.pendingAfter(runID, 1)
	if !ok {
		t.Fatal("expected run events after first")
	}
	if !afterFirst.Finished || len(afterFirst.Events) != 0 {
		t.Fatalf("unexpected event replay after first event: %#v", afterFirst)
	}
}

func TestRunManagerProviderErrorMarksRunFailedAndReleasesSession(t *testing.T) {
	manager := NewRunManager(nil)

	run, err := manager.startRun("session-1", message.UserTextMessage("fail", nil), nil, errorAgent())
	if err != nil {
		t.Fatal(err)
	}
	info := waitRunStatus(t, manager, run["id"].(string), "failed")
	if info["error"] != "upstream failed" {
		t.Fatalf("unexpected failed run info: %#v", info)
	}
	if manager.hasActiveRun("session-1") {
		t.Fatal("expected failed run to release active session")
	}
}

func TestRunManagerPermissionDecision(t *testing.T) {
	manager := NewRunManager(nil)
	agent := blockingAgent()
	run, err := manager.startRun("session-1", message.UserTextMessage("hello", nil), nil, agent)
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan permissions.ReviewDecision, 1)
	go func() {
		result <- manager.requestDecision(t.Context(), "session-1", permissions.ReviewRequest{
			ToolCallID: "call-1",
			ToolName:   "bash",
			Preview:    "go test ./...",
		})
	}()

	requestEvent := waitRunEvent(t, manager, run["id"].(string), "permission_request")
	requestID, _ := requestEvent.Data["request_id"].(string)

	if !manager.resolveDecision(run["id"].(string), requestID, permissions.ReviewAllow) {
		t.Fatal("expected decision to resolve")
	}
	if manager.resolveDecision(run["id"].(string), "missing", permissions.ReviewAllow) {
		t.Fatal("unexpected decision resolution for missing request")
	}
	if manager.resolveDecision("missing-run", requestID, permissions.ReviewAllow) {
		t.Fatal("unexpected decision resolution for missing run")
	}

	select {
	case decision := <-result:
		if decision != permissions.ReviewAllow {
			t.Fatalf("unexpected decision: %q", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("decision did not return")
	}

	resolvedEvent := waitRunEvent(t, manager, run["id"].(string), "permission_resolved")
	if resolvedEvent.Data["request_id"] != requestID || resolvedEvent.Data["decision"] != "allow" {
		t.Fatalf("unexpected permission resolution event: %#v", resolvedEvent)
	}

	close(agent.release)
}

func TestRunManagerCancelUnblocksPendingDecisionAsDeny(t *testing.T) {
	manager := NewRunManager(nil)
	run, err := manager.startRun("session-1", message.UserTextMessage("hello", nil), nil, blockingAgent())
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan permissions.ReviewDecision, 1)
	go func() {
		result <- manager.requestDecision(t.Context(), "session-1", permissions.ReviewRequest{
			ToolCallID: "call-1",
			ToolName:   "bash",
			Preview:    "go test ./...",
		})
	}()

	requestEvent := waitRunEvent(t, manager, run["id"].(string), "permission_request")
	requestID, _ := requestEvent.Data["request_id"].(string)

	cancelled := manager.cancelRun(run["id"].(string))
	if cancelled == nil || cancelled["status"] != "cancelled" {
		t.Fatalf("unexpected cancel result: %#v", cancelled)
	}

	select {
	case decision := <-result:
		if decision != permissions.ReviewDeny {
			t.Fatalf("unexpected decision: %q", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("decision did not return")
	}

	resolvedEvent := waitRunEvent(t, manager, run["id"].(string), "permission_resolved")
	if resolvedEvent.Data["request_id"] != requestID || resolvedEvent.Data["decision"] != "deny" {
		t.Fatalf("unexpected permission resolution event: %#v", resolvedEvent)
	}
}

func TestRunManagerPermissionDenyCancelsRun(t *testing.T) {
	manager := NewRunManager(nil)
	run, err := manager.startRun("session-1", message.UserTextMessage("hello", nil), nil, blockingAgent())
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan permissions.ReviewDecision, 1)
	go func() {
		result <- manager.requestDecision(t.Context(), "session-1", permissions.ReviewRequest{
			ToolCallID: "call-1",
			ToolName:   "bash",
			Preview:    "go test ./...",
		})
	}()

	requestEvent := waitRunEvent(t, manager, run["id"].(string), "permission_request")
	requestID, _ := requestEvent.Data["request_id"].(string)

	if !manager.resolveDecision(run["id"].(string), requestID, permissions.ReviewDeny) {
		t.Fatal("expected decision to resolve")
	}

	select {
	case decision := <-result:
		if decision != permissions.ReviewDeny {
			t.Fatalf("unexpected decision: %q", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("decision did not return")
	}

	waitRunStatus(t, manager, run["id"].(string), "cancelled")
	if manager.hasActiveRun("session-1") {
		t.Fatal("expected cancelled run to release active session")
	}
}
