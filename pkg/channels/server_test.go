package channels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestServerChannelInboundPublishesMessage(t *testing.T) {
	messageBus := bus.NewMessageBus()
	ch, err := NewServerChannel(config.ServerConfig{PathPrefix: "/api/server"}, messageBus)
	if err != nil {
		t.Fatalf("NewServerChannel error: %v", err)
	}

	ts := httptest.NewServer(ch.routes())
	defer ts.Close()

	body := `{"sender_id":"alice","chat_id":"chat-1","content":"hello from api"}`
	resp, err := http.Post(ts.URL+"/api/server/inbound", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST inbound error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("inbound status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	msg, ok := messageBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message")
	}

	if msg.Channel != "server" {
		t.Fatalf("inbound channel = %q, want %q", msg.Channel, "server")
	}
	if msg.ChatID != "chat-1" {
		t.Fatalf("inbound chat_id = %q, want %q", msg.ChatID, "chat-1")
	}
	if msg.Content != "hello from api" {
		t.Fatalf("inbound content = %q, want %q", msg.Content, "hello from api")
	}
}

func TestServerChannelOutboundLongPoll(t *testing.T) {
	messageBus := bus.NewMessageBus()
	ch, err := NewServerChannel(config.ServerConfig{PathPrefix: "/api/server"}, messageBus)
	if err != nil {
		t.Fatalf("NewServerChannel error: %v", err)
	}
	ch.setRunning(true)

	ts := httptest.NewServer(ch.routes())
	defer ts.Close()

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = ch.Send(context.Background(), bus.OutboundMessage{
			Channel: "server",
			ChatID:  "chat-42",
			Content: "assistant reply",
		})
	}()

	resp, err := http.Get(ts.URL + "/api/server/outbound?chat_id=chat-42&timeout_sec=2")
	if err != nil {
		t.Fatalf("GET outbound error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("outbound status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var payload outboundResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode outbound response: %v", err)
	}

	if len(payload.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(payload.Messages))
	}
	if payload.Messages[0].Message.Content != "assistant reply" {
		t.Fatalf("content = %q, want %q", payload.Messages[0].Message.Content, "assistant reply")
	}
}

func TestServerChannelAuthToken(t *testing.T) {
	messageBus := bus.NewMessageBus()
	ch, err := NewServerChannel(config.ServerConfig{PathPrefix: "/api/server", AuthToken: "secret"}, messageBus)
	if err != nil {
		t.Fatalf("NewServerChannel error: %v", err)
	}

	ts := httptest.NewServer(ch.routes())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/server/inbound", strings.NewReader(`{"content":"hi"}`))
	if err != nil {
		t.Fatalf("new request error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestServerChannelUI(t *testing.T) {
	messageBus := bus.NewMessageBus()
	ch, err := NewServerChannel(config.ServerConfig{PathPrefix: "/api/server"}, messageBus)
	if err != nil {
		t.Fatalf("NewServerChannel error: %v", err)
	}

	ts := httptest.NewServer(ch.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/server/ui")
	if err != nil {
		t.Fatalf("GET ui error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ui status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read ui body error: %v", err)
	}

	html := string(body)
	if !strings.Contains(html, "data-prefix=\"/api/server\"") {
		t.Fatalf("ui should include configured route prefix")
	}
	if !strings.Contains(html, "'/inbound'") {
		t.Fatalf("ui should include inbound API usage")
	}
}
