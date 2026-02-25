package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	defaultServerPathPrefix = "/api/server"
	defaultPollTimeout      = 30 * time.Second
	maxPollTimeout          = 120 * time.Second
	defaultQueueSize        = 100
	maxQueueSize            = 1000
)

type queuedOutbound struct {
	Message    bus.OutboundMessage `json:"message"`
	EnqueuedAt time.Time           `json:"enqueued_at"`
}

type outboundResponse struct {
	Messages []queuedOutbound `json:"messages"`
	HasMore  bool             `json:"has_more"`
}

type ServerChannel struct {
	*BaseChannel
	config         config.ServerConfig
	httpServer     *http.Server
	routePrefix    string
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	outboundQueues map[string][]queuedOutbound
	waiters        map[string]map[chan struct{}]struct{}
	maxQueue       int
}

func NewServerChannel(cfg config.ServerConfig, messageBus *bus.MessageBus) (*ServerChannel, error) {
	prefix := strings.TrimSpace(cfg.PathPrefix)
	if prefix == "" {
		prefix = defaultServerPathPrefix
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimRight(prefix, "/")

	queueSize := cfg.MaxQueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	if queueSize > maxQueueSize {
		queueSize = maxQueueSize
	}

	base := NewBaseChannel("server", cfg, messageBus, cfg.AllowFrom)

	return &ServerChannel{
		BaseChannel:    base,
		config:         cfg,
		routePrefix:    prefix,
		outboundQueues: make(map[string][]queuedOutbound),
		waiters:        make(map[string]map[chan struct{}]struct{}),
		maxQueue:       queueSize,
	}, nil
}

func (c *ServerChannel) Start(ctx context.Context) error {
	if c.IsRunning() {
		return nil
	}

	c.ctx, c.cancel = context.WithCancel(ctx)
	mux := c.routes()
	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)
	c.httpServer = &http.Server{Addr: addr, Handler: mux}

	go func() {
		logger.InfoCF("server", "Server channel listening", map[string]interface{}{
			"addr":        addr,
			"path_prefix": c.routePrefix,
		})
		if err := c.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.ErrorCF("server", "Server channel HTTP error", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}()

	c.setRunning(true)
	return nil
}

func (c *ServerChannel) Stop(ctx context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}

	if c.httpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := c.httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}

	c.setRunning(false)
	return nil
}

func (c *ServerChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("server channel not running")
	}

	queued := queuedOutbound{
		Message:    msg,
		EnqueuedAt: time.Now().UTC(),
	}

	c.mu.Lock()
	queue := append(c.outboundQueues[msg.ChatID], queued)
	if len(queue) > c.maxQueue {
		queue = queue[len(queue)-c.maxQueue:]
	}
	c.outboundQueues[msg.ChatID] = queue

	waiters := c.waiters[msg.ChatID]
	for ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	c.mu.Unlock()

	return nil
}

func (c *ServerChannel) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(c.routePrefix, c.handleUI)
	mux.HandleFunc(c.routePrefix+"/", c.handleUI)
	mux.HandleFunc(c.routePrefix+"/ui", c.handleUI)
	mux.HandleFunc(c.routePrefix+"/health", c.handleHealth)
	mux.HandleFunc(c.routePrefix+"/inbound", c.handleInbound)
	mux.HandleFunc(c.routePrefix+"/outbound", c.handleOutbound)
	return mux
}

func (c *ServerChannel) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path != c.routePrefix && r.URL.Path != c.routePrefix+"/" && r.URL.Path != c.routePrefix+"/ui" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ui := renderServerUI(c.routePrefix)
	_, _ = w.Write([]byte(ui))
}

func (c *ServerChannel) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "ok",
		"channel":     "server",
		"path_prefix": c.routePrefix,
	})
}

func (c *ServerChannel) handleInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !c.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		SenderID string            `json:"sender_id"`
		ChatID   string            `json:"chat_id"`
		Content  string            `json:"content"`
		Media    []string          `json:"media,omitempty"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	req.ChatID = strings.TrimSpace(req.ChatID)
	if req.ChatID == "" {
		req.ChatID = "default"
	}

	req.SenderID = strings.TrimSpace(req.SenderID)
	if req.SenderID == "" {
		req.SenderID = "server-api"
	}

	if !c.IsAllowed(req.SenderID) {
		http.Error(w, "sender not allowed", http.StatusForbidden)
		return
	}

	c.HandleMessage(req.SenderID, req.ChatID, req.Content, req.Media, req.Metadata)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":      "queued",
		"channel":     "server",
		"chat_id":     req.ChatID,
		"session_key": fmt.Sprintf("server:%s", req.ChatID),
	})
}

func (c *ServerChannel) handleOutbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !c.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	chatID := strings.TrimSpace(r.URL.Query().Get("chat_id"))
	if chatID == "" {
		http.Error(w, "chat_id is required", http.StatusBadRequest)
		return
	}

	limit := parseIntParam(r.URL.Query().Get("limit"), 1)
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}

	timeout := parseDurationParam(r.URL.Query().Get("timeout_sec"), defaultPollTimeout)
	if timeout > maxPollTimeout {
		timeout = maxPollTimeout
	}
	if timeout < 0 {
		timeout = 0
	}

	messages, hasMore := c.popMessages(chatID, limit)
	if len(messages) == 0 && timeout > 0 {
		notify := c.registerWaiter(chatID)
		defer c.unregisterWaiter(chatID, notify)

		select {
		case <-notify:
			messages, hasMore = c.popMessages(chatID, limit)
		case <-time.After(timeout):
		case <-r.Context().Done():
			return
		}
	}

	writeJSON(w, http.StatusOK, outboundResponse{Messages: messages, HasMore: hasMore})
}

func (c *ServerChannel) authorized(r *http.Request) bool {
	token := strings.TrimSpace(c.config.AuthToken)
	if token == "" {
		return true
	}

	if r.Header.Get("X-API-Key") == token {
		return true
	}

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) == token
	}

	return false
}

func (c *ServerChannel) registerWaiter(chatID string) chan struct{} {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	if c.waiters[chatID] == nil {
		c.waiters[chatID] = make(map[chan struct{}]struct{})
	}
	c.waiters[chatID][ch] = struct{}{}
	c.mu.Unlock()
	return ch
}

func (c *ServerChannel) unregisterWaiter(chatID string, ch chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	waiters := c.waiters[chatID]
	if waiters == nil {
		return
	}
	delete(waiters, ch)
	if len(waiters) == 0 {
		delete(c.waiters, chatID)
	}
}

func (c *ServerChannel) popMessages(chatID string, limit int) ([]queuedOutbound, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	queue := c.outboundQueues[chatID]
	if len(queue) == 0 {
		return nil, false
	}

	if limit > len(queue) {
		limit = len(queue)
	}

	messages := append([]queuedOutbound(nil), queue[:limit]...)
	remaining := queue[limit:]
	if len(remaining) == 0 {
		delete(c.outboundQueues, chatID)
	} else {
		c.outboundQueues[chatID] = remaining
	}

	return messages, len(remaining) > 0
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func parseIntParam(raw string, fallback int) int {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func parseDurationParam(raw string, fallback time.Duration) time.Duration {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return time.Duration(v) * time.Second
}

func renderServerUI(routePrefix string) string {
	const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>PicoClaw Server Chat</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f4f7fb;
      --panel: #ffffff;
      --text: #1f2937;
      --muted: #6b7280;
      --border: #d1d5db;
      --accent: #0b6a88;
      --accent-soft: #e7f4f9;
    }
    * { box-sizing: border-box; }
    html, body {
      width: 100%%;
      height: 100%%;
      overflow: hidden;
    }
    body {
      margin: 0;
      font-family: "Iosevka Aile", "IBM Plex Sans", "Noto Sans", sans-serif;
      background: radial-gradient(circle at top right, #eaf3ff 0, #f4f7fb 45%%, #edf7fb 100%%);
      color: var(--text);
      min-height: 100dvh;
      display: block;
      padding: 0;
    }
    .app {
      width: 100%%;
      background: var(--panel);
      border: none;
      border-radius: 0;
      box-shadow: none;
      overflow: hidden;
      display: grid;
      grid-template-rows: auto auto 1fr auto;
      min-height: 100dvh;
      height: 100dvh;
    }
    .topwrap {
      border-bottom: 1px solid var(--border);
      background: linear-gradient(90deg, #f7fbff 0%%, #f1f7fb 100%%);
    }
    .topbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 10px 12px;
    }
    .title { font-weight: 700; letter-spacing: 0.01em; }
    .toggle {
      border: 1px solid var(--border);
      background: #fff;
      color: var(--text);
      padding: 6px 10px;
      border-radius: 8px;
      cursor: pointer;
      font-size: 13px;
    }
    .top {
      padding: 12px;
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: 8px;
    }
    .top.collapsed { display: none; }
    .field {
      display: flex;
      flex-direction: column;
      gap: 4px;
      font-size: 12px;
      color: var(--muted);
    }
    input, textarea, button {
      font: inherit;
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 8px 10px;
      background: #fff;
    }
    button {
      cursor: pointer;
      background: var(--accent);
      color: white;
      border: none;
      font-weight: 600;
    }
    button.secondary { background: #64748b; }
    .chat {
      padding: 14px;
      overflow: auto;
      display: flex;
      flex-direction: column;
      gap: 10px;
      background: #fcfdfd;
    }
    .msg {
      max-width: 90%%;
      padding: 10px 12px;
      border-radius: 10px;
      border: 1px solid var(--border);
      line-height: 1.4;
      animation: fadein 160ms ease-out;
    }
    .msg p { margin: 0 0 8px; }
    .msg p:last-child { margin-bottom: 0; }
    .msg code {
      background: rgba(148, 163, 184, 0.2);
      padding: 1px 5px;
      border-radius: 4px;
      font-family: "Iosevka Term", "JetBrains Mono", monospace;
      font-size: 0.92em;
    }
    .msg pre {
      margin: 8px 0;
      background: #0f172a;
      color: #e2e8f0;
      padding: 10px;
      border-radius: 8px;
      overflow-x: auto;
    }
    .msg pre code {
      background: transparent;
      padding: 0;
      border-radius: 0;
      color: inherit;
    }
    .msg.user {
      align-self: flex-end;
      background: var(--accent-soft);
      border-color: #b4dfdb;
      white-space: pre-wrap;
    }
    .msg.assistant {
      align-self: flex-start;
      background: #fff;
    }
    .msg.typing {
      align-self: flex-start;
      background: #fffaf0;
      border-color: #f2d9ac;
      color: #7a5b1b;
      display: none;
    }
    .dots {
      display: inline-flex;
      gap: 3px;
      margin-left: 6px;
      vertical-align: middle;
    }
    .dots span {
      width: 5px;
      height: 5px;
      border-radius: 99px;
      background: #b38728;
      animation: blink 1s infinite ease-in-out;
    }
    .dots span:nth-child(2) { animation-delay: 0.15s; }
    .dots span:nth-child(3) { animation-delay: 0.3s; }
    .composer {
      border-top: 1px solid var(--border);
      display: grid;
      grid-template-columns: 1fr auto;
      gap: 8px;
      padding: 10px;
      background: #fff;
    }
    #status {
      padding: 6px 10px;
      font-size: 12px;
      color: var(--muted);
      border-bottom: 1px solid var(--border);
      background: #fafafa;
    }
    @media (max-width: 740px) {
      .top { grid-template-columns: 1fr; }
      .composer { grid-template-columns: 1fr; }
      button { width: 100%%; }
      .msg { max-width: 96%%; }
      textarea { min-height: 92px; }
    }
    @media (min-width: 741px) {
      body {
        display: flex;
        justify-content: center;
        padding: 8px;
      }
      .app {
        width: min(900px, 100%%);
        min-height: 95dvh;
        height: 95dvh;
        border: 1px solid var(--border);
        border-radius: 14px;
        box-shadow: 0 12px 28px rgba(15, 23, 42, 0.08);
      }
    }
    @keyframes fadein {
      from { opacity: 0; transform: translateY(3px); }
      to { opacity: 1; transform: translateY(0); }
    }
    @keyframes blink {
      0%%, 80%%, 100%% { opacity: 0.25; transform: translateY(0); }
      40%% { opacity: 1; transform: translateY(-2px); }
    }
  </style>
</head>
<body>
  <div class="app" data-prefix="{{.Prefix}}">
    <div class="topwrap">
      <div class="topbar">
        <div class="title">PicoClaw Chat</div>
        <button class="toggle" id="toggle-settings" type="button">Show Settings</button>
      </div>
      <div class="top collapsed" id="settings-panel">
        <label class="field">Chat ID<input id="chat" value="default" /></label>
        <label class="field">Sender ID<input id="sender" value="web-user" /></label>
        <label class="field">API Token<input id="token" type="password" placeholder="optional" /></label>
        <label class="field">Actions
          <div style="display:flex; gap:8px;">
            <button id="connect" type="button">Connect</button>
            <button id="clear" type="button" class="secondary">Clear</button>
          </div>
        </label>
      </div>
    </div>
    <div id="status">Idle</div>
    <div class="chat" id="chatlog">
      <div id="typing" class="msg typing">
        PicoClaw is typing
        <span class="dots"><span></span><span></span><span></span></span>
      </div>
    </div>
    <div class="composer">
      <textarea id="content" rows="3" placeholder="Type a message..."></textarea>
      <div style="display:flex; gap:8px; align-items:stretch;">
        <button id="send" type="button">Send</button>
        <button id="stop" type="button" class="secondary">Stop</button>
      </div>
    </div>
  </div>

  <script>
    const app = document.querySelector('.app');
    const prefix = app.dataset.prefix;
    const chatEl = document.getElementById('chat');
    const senderEl = document.getElementById('sender');
    const tokenEl = document.getElementById('token');
    const statusEl = document.getElementById('status');
    const chatlogEl = document.getElementById('chatlog');
    const contentEl = document.getElementById('content');
    const sendBtn = document.getElementById('send');
    const stopBtn = document.getElementById('stop');
    const connectBtn = document.getElementById('connect');
    const clearBtn = document.getElementById('clear');
    const typingEl = document.getElementById('typing');
    const settingsPanel = document.getElementById('settings-panel');
    const toggleSettingsBtn = document.getElementById('toggle-settings');

    let polling = false;
    let waitingForReply = false;

    function historyStorageKey() {
      const chatId = (chatEl.value.trim() || 'default').replace(/\s+/g, '_');
      return 'picoclaw_server_history_' + chatId;
    }

    function loadHistory() {
      try {
        const raw = localStorage.getItem(historyStorageKey());
        if (!raw) return [];
        const parsed = JSON.parse(raw);
        if (!Array.isArray(parsed)) return [];
        return parsed.filter((item) => item && (item.role === 'user' || item.role === 'assistant') && typeof item.text === 'string');
      } catch (_) {
        return [];
      }
    }

    function saveHistory(history) {
      try {
        localStorage.setItem(historyStorageKey(), JSON.stringify(history.slice(-200)));
      } catch (_) {}
    }

    function appendHistory(role, text) {
      const history = loadHistory();
      history.push({ role, text, ts: Date.now() });
      saveHistory(history);
    }

    function clearRenderedMessages() {
      for (const el of [...chatlogEl.querySelectorAll('.msg')]) {
        if (el.id !== 'typing') el.remove();
      }
    }

    function renderStoredHistory() {
      clearRenderedMessages();
      const history = loadHistory();
      for (const item of history) {
        addMessage(item.role, item.text, false);
      }
    }

    function setStatus(text) { statusEl.textContent = text; }

    function authHeaders() {
      const token = tokenEl.value.trim();
      if (!token) return {};
      return { 'X-API-Key': token };
    }

    function escapeHtml(text) {
      return text
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    }

    function renderMarkdown(text) {
      const lines = text.replace(/\r/g, '').split('\n');
      let html = '';
      let inCode = false;
      let inList = false;
      const backtick = String.fromCharCode(96);
      const tripleBacktick = backtick + backtick + backtick;

      function closeList() {
        if (inList) {
          html += '</ul>';
          inList = false;
        }
      }

      function inlineMarkdown(value) {
        let s = escapeHtml(value);
        s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
        s = s.replace(new RegExp('\\x60([^\\x60]+)\\x60', 'g'), '<code>$1</code>');
        s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
        s = s.replace(/(^|\s)\*([^*\n]+)\*(?=\s|$)/g, '$1<em>$2</em>');
        return s;
      }

      for (const line of lines) {
        if (line.trim().startsWith(tripleBacktick)) {
          closeList();
          if (!inCode) {
            inCode = true;
            html += '<pre><code>';
          } else {
            inCode = false;
            html += '</code></pre>';
          }
          continue;
        }
        if (inCode) {
          html += escapeHtml(line) + '\n';
          continue;
        }

        const trimmed = line.trim();
        if (!trimmed) {
          closeList();
          continue;
        }

        if (trimmed.startsWith('- ')) {
          if (!inList) {
            html += '<ul>';
            inList = true;
          }
          html += '<li>' + inlineMarkdown(trimmed.slice(2)) + '</li>';
          continue;
        }

        closeList();
        if (trimmed.startsWith('### ')) {
          html += '<p><strong>' + inlineMarkdown(trimmed.slice(4)) + '</strong></p>';
        } else if (trimmed.startsWith('## ')) {
          html += '<p><strong>' + inlineMarkdown(trimmed.slice(3)) + '</strong></p>';
        } else if (trimmed.startsWith('# ')) {
          html += '<p><strong>' + inlineMarkdown(trimmed.slice(2)) + '</strong></p>';
        } else {
          html += '<p>' + inlineMarkdown(trimmed) + '</p>';
        }
      }

      closeList();
      if (inCode) html += '</code></pre>';
      return html || '<p></p>';
    }

    function showTyping() {
      typingEl.style.display = waitingForReply ? 'block' : 'none';
      if (waitingForReply) {
        chatlogEl.appendChild(typingEl);
        chatlogEl.scrollTop = chatlogEl.scrollHeight;
      }
    }

    function addMessage(role, text, persist = true) {
      const node = document.createElement('div');
      node.className = 'msg ' + role;
      if (role === 'assistant') {
        node.innerHTML = renderMarkdown(text || '');
      } else {
        node.textContent = text;
      }
      chatlogEl.insertBefore(node, typingEl);
      chatlogEl.scrollTop = chatlogEl.scrollHeight;
      if (persist) {
        appendHistory(role, text || '');
      }
    }

    async function sendMessage() {
      const content = contentEl.value.trim();
      if (!content) return;
      const payload = {
        sender_id: senderEl.value.trim() || 'web-user',
        chat_id: chatEl.value.trim() || 'default',
        content
      };

      addMessage('user', content, true);
      contentEl.value = '';
      setStatus('Sending...');
      waitingForReply = true;
      showTyping();

      const res = await fetch(prefix + '/inbound', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify(payload)
      });

      if (!res.ok) {
        const txt = await res.text();
        waitingForReply = false;
        showTyping();
        addMessage('assistant', 'Request failed: ' + txt);
      }
      setStatus('Waiting for response...');
    }

    async function pollOnce() {
      const chatId = chatEl.value.trim() || 'default';
      const url = new URL(prefix + '/outbound', window.location.origin);
      url.searchParams.set('chat_id', chatId);
      url.searchParams.set('timeout_sec', '25');
      url.searchParams.set('limit', '5');

      const res = await fetch(url, { headers: authHeaders() });
      if (!res.ok) {
        const txt = await res.text();
        throw new Error(txt || ('HTTP ' + res.status));
      }

      const data = await res.json();
      const messages = data.messages || [];
      if (messages.length > 0) {
        waitingForReply = false;
        showTyping();
      }
      for (const item of messages) {
        addMessage('assistant', item.message.content || '', true);
      }
      return messages.length;
    }

    async function resetConversation() {
      const payload = {
        sender_id: senderEl.value.trim() || 'web-user',
        chat_id: chatEl.value.trim() || 'default',
        content: '/new'
      };

      const res = await fetch(prefix + '/inbound', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeaders() },
        body: JSON.stringify(payload)
      });

      if (!res.ok) {
        const txt = await res.text();
        throw new Error(txt || ('HTTP ' + res.status));
      }
    }

    async function pollLoop() {
      if (polling) return;
      polling = true;
      setStatus('Connected');
      while (polling) {
        try {
          const count = await pollOnce();
          if (count === 0) setStatus(waitingForReply ? 'PicoClaw is typing...' : 'Connected (listening...)');
          else setStatus('Connected (' + count + ' new message' + (count > 1 ? 's' : '') + ')');
        } catch (err) {
          setStatus('Polling error: ' + err.message + ' (retrying)');
          await new Promise((r) => setTimeout(r, 1200));
        }
      }
    }

    toggleSettingsBtn.addEventListener('click', () => {
      settingsPanel.classList.toggle('collapsed');
      toggleSettingsBtn.textContent = settingsPanel.classList.contains('collapsed') ? 'Show Settings' : 'Hide Settings';
    });

    connectBtn.addEventListener('click', () => {
      pollLoop();
    });

    chatEl.addEventListener('change', () => {
      renderStoredHistory();
    });

    chatEl.addEventListener('blur', () => {
      renderStoredHistory();
    });

    stopBtn.addEventListener('click', () => {
      polling = false;
      waitingForReply = false;
      showTyping();
      setStatus('Stopped');
    });

    clearBtn.addEventListener('click', async () => {
      setStatus('Clearing conversation...');
      try {
        await resetConversation();
        localStorage.removeItem(historyStorageKey());
        clearRenderedMessages();
        waitingForReply = false;
        showTyping();
        setStatus('Conversation cleared.');
        pollLoop();
      } catch (err) {
        setStatus('Clear error: ' + err.message);
      }
    });

    sendBtn.addEventListener('click', async () => {
      try {
        await sendMessage();
        pollLoop();
      } catch (err) {
        waitingForReply = false;
        showTyping();
        setStatus('Send error: ' + err.message);
      }
    });

    contentEl.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        sendBtn.click();
      }
    });

    renderStoredHistory();
  </script>
</body>
</html>`

	tmpl := template.Must(template.New("server-ui").Parse(page))
	var sb strings.Builder
	_ = tmpl.Execute(&sb, map[string]string{"Prefix": routePrefix})
	return sb.String()
}
