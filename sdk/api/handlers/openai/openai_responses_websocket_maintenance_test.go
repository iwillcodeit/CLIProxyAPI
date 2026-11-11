package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/maintenance"
)

// A create queued behind an unacknowledged steer passed the reader before the
// drain began. When it is flushed, it must be refused without reaching upstream.
func TestMaintenanceRejectsQueuedDuplexCreate(t *testing.T) {
	steerSeen, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
		write := func(s string) { _ = c.WriteMessage(websocket.TextMessage, []byte(s)) }
		_, _, _ = c.ReadMessage() // initial create
		write(`{"type":"response.created","response":{"id":"r1"}}`)
		_, _, _ = c.ReadMessage() // steer
		close(steerSeen)
		<-release
		write(`{"type":"response.steer.accepted","steer":{"id":"s1","previous_response_id":"r1"}}`)
		write(`{"type":"response.incomplete","response":{"id":"r1","incomplete_details":{"reason":"steered"},"output":[]}}`)
		write(`{"type":"response.created","response":{"id":"r2"}}`)
		write(`{"type":"response.completed","response":{"id":"r2","output":[]}}`)
		if _, b, err := c.ReadMessage(); err == nil && ojson.GetBytesNoCopy(b, "type").String() == "response.create" {
			t.Errorf("queued create reached upstream during maintenance: %s", b)
		}
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.Codex.ResponseSteering = true
	cfg.CodexResponseSteering = true
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	authID := "maintenance-queued-create"
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"}}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "maintenance-test-model"}})
	defer registry.GetGlobalRegistry().UnregisterClient(authID)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	gate := maintenance.New()
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		finish, err := gate.Begin()
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		defer finish()
		c.Request = c.Request.WithContext(maintenance.WithRequest(c.Request.Context(), gate, finish))
		h.ResponsesWebsocket(c)
	})
	downstream := httptest.NewServer(router)
	defer downstream.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	send := func(s string) {
		if err := c.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"type":"response.create","model":"maintenance-test-model","input":[]}`)
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("downstream read: %v", err)
		}
		kind := ojson.GetBytesNoCopy(b, "type").String()
		switch {
		case kind == "response.created" && ojson.GetBytesNoCopy(b, "response.id").String() == "r1":
			send(`{"type":"response.steer","previous_response_id":"r1","input":"one"}`)
			<-steerSeen
			send(`{"type":"response.create","model":"maintenance-test-model","input":[]}`)
			// The reader answers a malformed interrupt only after admitting the create.
			send(`{"type":"response.interrupt"}`)
		case kind == "error" && ojson.GetBytesNoCopy(b, "status").Int() == http.StatusBadRequest:
			if _, errDrain := gate.Drain(strings.Repeat("t", 32)); errDrain != nil {
				t.Fatal(errDrain)
			}
			close(release)
		case kind == "error":
			if ojson.GetBytesNoCopy(b, "status").Int() != http.StatusServiceUnavailable {
				t.Fatalf("unexpected error: %s", b)
			}
			_ = c.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not finish")
			}
			return
		}
	}
}
