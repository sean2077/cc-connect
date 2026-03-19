package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// mockUpdaterPlatform implements Platform + MessageUpdater + PreviewStarter.
type mockUpdaterPlatform struct {
	stubPlatformEngine
	mu       sync.Mutex
	messages []string // track all sent/updated messages
	lastMsg  string
}

func (m *mockUpdaterPlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, "start:"+content)
	m.lastMsg = content
	return "preview-handle", nil
}

func (m *mockUpdaterPlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, "update:"+content)
	m.lastMsg = content
	return nil
}

func (m *mockUpdaterPlatform) getMessages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.messages))
	copy(out, m.messages)
	return out
}

func TestStreamPreview_BasicFlow(t *testing.T) {
	mp := &mockUpdaterPlatform{}
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    100,
		MinDeltaChars: 5,
		MaxChars:      500,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())

	if !sp.canPreview() {
		t.Fatal("should be able to preview")
	}

	sp.appendText("Hello ")
	time.Sleep(150 * time.Millisecond)

	msgs := mp.getMessages()
	if len(msgs) == 0 {
		t.Fatal("expected at least one message sent")
	}
	if msgs[0] != "start:Hello " {
		t.Errorf("first message = %q, want 'start:Hello '", msgs[0])
	}
}

func TestStreamPreview_ThrottlesUpdates(t *testing.T) {
	mp := &mockUpdaterPlatform{}
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    200,
		MinDeltaChars: 5,
		MaxChars:      500,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())

	// Rapid-fire small appends
	for i := 0; i < 10; i++ {
		sp.appendText("ab")
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for throttle timers to fire
	time.Sleep(300 * time.Millisecond)

	msgs := mp.getMessages()
	// Should NOT have 10 individual updates; throttling should batch them
	if len(msgs) >= 10 {
		t.Errorf("expected throttling to reduce updates, got %d", len(msgs))
	}
	if len(msgs) == 0 {
		t.Error("expected at least one update")
	}
}

func TestStreamPreview_MaxChars(t *testing.T) {
	mp := &mockUpdaterPlatform{}
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    50,
		MinDeltaChars: 1,
		MaxChars:      10,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())
	sp.appendText("This is a very long text that exceeds max chars limit")
	time.Sleep(100 * time.Millisecond)

	msgs := mp.getMessages()
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	// Last message should be truncated
	for _, m := range msgs {
		if len(m) > 0 {
			// Content after "start:" or "update:" should respect maxChars
			content := m
			for _, prefix := range []string{"start:", "update:"} {
				if len(content) > len(prefix) && content[:len(prefix)] == prefix {
					content = content[len(prefix):]
				}
			}
			if len([]rune(content)) > 15 { // 10 chars + "…" with some margin
				t.Errorf("message too long: %q (%d runes)", content, len([]rune(content)))
			}
		}
	}
}

func TestStreamPreview_Disabled(t *testing.T) {
	mp := &mockUpdaterPlatform{}
	cfg := StreamPreviewCfg{Enabled: false}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())
	if sp.canPreview() {
		t.Error("should not be able to preview when disabled")
	}

	sp.appendText("Hello")
	time.Sleep(50 * time.Millisecond)

	msgs := mp.getMessages()
	if len(msgs) != 0 {
		t.Error("no messages should be sent when disabled")
	}
}

func TestStreamPreview_FinishInPlace(t *testing.T) {
	mp := &mockUpdaterPlatform{}
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    50,
		MinDeltaChars: 1,
		MaxChars:      500,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())
	sp.appendText("Hello World")
	time.Sleep(100 * time.Millisecond)

	ok := sp.finish("Hello World Final")
	if !ok {
		t.Error("finish should return true when preview was active")
	}

	msgs := mp.getMessages()
	last := msgs[len(msgs)-1]
	if last != "update:Hello World Final" {
		t.Errorf("last message = %q, want 'update:Hello World Final'", last)
	}
}

// mockCleanerPlatform adds PreviewCleaner to mockUpdaterPlatform.
type mockCleanerPlatform struct {
	mockUpdaterPlatform
	deleted []any
}

func (m *mockCleanerPlatform) DeletePreviewMessage(_ context.Context, handle any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, handle)
	return nil
}

func TestStreamPreview_FreezeDeletesOnFinish(t *testing.T) {
	mp := &mockCleanerPlatform{}
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    50,
		MinDeltaChars: 1,
		MaxChars:      500,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())
	sp.appendText("Hello World")
	time.Sleep(100 * time.Millisecond)

	// Simulate a tool/thinking event → freeze
	sp.freeze()

	// finish should return false (degraded) and delete the stale preview
	ok := sp.finish("Hello World Final")
	if ok {
		t.Error("finish should return false when degraded")
	}

	mp.mu.Lock()
	deletedCount := len(mp.deleted)
	mp.mu.Unlock()
	if deletedCount != 1 {
		t.Errorf("expected 1 delete call, got %d", deletedCount)
	}
}

// mockFailingUpdaterPlatform fails UpdateMessage after N successful calls.
type mockFailingUpdaterPlatform struct {
	stubPlatformEngine
	mu            sync.Mutex
	messages      []string
	lastMsg       string
	updateCalls   int
	failAfterN    int // UpdateMessage fails after this many successful calls (0 = never fail)
	alwaysFailUpd bool
}

func (m *mockFailingUpdaterPlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, "start:"+content)
	m.lastMsg = content
	return "preview-handle", nil
}

func (m *mockFailingUpdaterPlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	if m.alwaysFailUpd || (m.failAfterN > 0 && m.updateCalls > m.failAfterN) {
		m.messages = append(m.messages, "update-fail:"+content)
		return errors.New("mock update error")
	}
	m.messages = append(m.messages, "update:"+content)
	m.lastMsg = content
	return nil
}

func (m *mockFailingUpdaterPlatform) getMessages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.messages))
	copy(out, m.messages)
	return out
}

// TestStreamPreview_FinishNoopUpdateTreatedAsSuccess verifies that when
// UpdateMessage fails but the preview already shows the same text (e.g. Feishu
// Patch API rejecting identical content), finish() returns true to prevent
// the engine from sending a duplicate message via p.Send().
func TestStreamPreview_FinishNoopUpdateTreatedAsSuccess(t *testing.T) {
	mp := &mockFailingUpdaterPlatform{failAfterN: 1} // first UpdateMessage ok, second fails
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    50,
		MinDeltaChars: 1,
		MaxChars:      500,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())
	sp.appendText("Hello World")
	time.Sleep(100 * time.Millisecond) // let first flush (SendPreviewStart) happen

	// Wait for timer to fire so UpdateMessage is called once (succeeds)
	sp.appendText(" more")
	time.Sleep(100 * time.Millisecond)

	msgs := mp.getMessages()
	// Should have: start + update
	hasUpdate := false
	var lastContent string
	for _, m := range msgs {
		if len(m) > 7 && m[:7] == "update:" {
			hasUpdate = true
			lastContent = m[7:]
		}
	}
	if !hasUpdate {
		t.Fatal("expected at least one successful UpdateMessage before testing finish")
	}

	// Now finish with the SAME text that was last sent via UpdateMessage.
	// The second UpdateMessage call will fail, but since text matches
	// lastSentText, finish() should return true.
	ok := sp.finish(lastContent)
	if !ok {
		t.Error("finish should return true when UpdateMessage fails but content matches lastSentText")
	}
}

// TestStreamPreview_DegradedContentMatchReturnsTrueWithoutCleaner verifies
// that when a preview is degraded and the platform has no PreviewCleaner,
// finish() returns true if the final text matches what was last sent —
// preventing duplicate messages on platforms like Feishu.
func TestStreamPreview_DegradedContentMatchReturnsTrueWithoutCleaner(t *testing.T) {
	mp := &mockFailingUpdaterPlatform{failAfterN: 1}
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    50,
		MinDeltaChars: 1,
		MaxChars:      500,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())
	sp.appendText("Hello World")
	time.Sleep(100 * time.Millisecond)

	// Freeze the preview (simulates a tool call / permission prompt)
	// This updates the preview one last time and marks it degraded.
	sp.freeze()

	// finish with exactly the text that was last sent before freeze.
	// Since degraded=true and no PreviewCleaner, but content matches,
	// finish should return true.
	ok := sp.finish("Hello World")
	if !ok {
		t.Error("finish should return true when degraded but content matches (no PreviewCleaner)")
	}
}

// TestStreamPreview_DegradedRecoveryViaUpdateMessage verifies that when a
// preview is degraded (without PreviewCleaner) and the final text differs
// from what was displayed, finish() attempts a last-resort UpdateMessage
// to recover the preview.
func TestStreamPreview_DegradedRecoveryViaUpdateMessage(t *testing.T) {
	// failAfterN=1: first UpdateMessage succeeds, second fails (causing degradation),
	// but the third (recovery attempt in finish) should succeed because we reset.
	mp := &mockFailingUpdaterPlatform{failAfterN: 100} // won't fail during test
	cfg := StreamPreviewCfg{
		Enabled:       true,
		IntervalMs:    50,
		MinDeltaChars: 1,
		MaxChars:      500,
	}

	sp := newStreamPreview(cfg, mp, "ctx", context.Background())
	sp.appendText("partial")
	time.Sleep(100 * time.Millisecond)

	// Manually degrade the preview to simulate a failed update during streaming
	sp.mu.Lock()
	sp.degraded = true
	sp.mu.Unlock()

	// finish with different text — should attempt recovery via UpdateMessage
	ok := sp.finish("partial plus more text")
	if !ok {
		t.Error("finish should return true when degraded preview is recovered via UpdateMessage")
	}

	msgs := mp.getMessages()
	found := false
	for _, m := range msgs {
		if m == "update:partial plus more text" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected recovery UpdateMessage, got messages: %v", msgs)
	}
}

func TestStreamPreview_NonUpdaterPlatform(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	cfg := DefaultStreamPreviewCfg()

	sp := newStreamPreview(cfg, p, "ctx", context.Background())
	if sp.canPreview() {
		t.Error("should not preview on non-updater platform")
	}
}
