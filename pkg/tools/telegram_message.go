package tools

import (
	"context"
	"fmt"
)

type TelegramSendCallback func(chatID, content, attachmentPath string) error

type TelegramMessageTool struct {
	sendCallback  TelegramSendCallback
	defaultChatID string
	sentInRound   bool
}

func NewTelegramMessageTool() *TelegramMessageTool {
	return &TelegramMessageTool{}
}

func (t *TelegramMessageTool) Name() string {
	return "telegram_message"
}

func (t *TelegramMessageTool) Description() string {
	return "Send a Telegram message to the user, optionally with an attachment file path."
}

func (t *TelegramMessageTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The message content to send",
			},
			"chat_id": map[string]interface{}{
				"type":        "string",
				"description": "Optional: target Telegram chat ID; defaults to current chat",
			},
			"attachment_path": map[string]interface{}{
				"type":        "string",
				"description": "Optional: local file path to send as Telegram attachment",
			},
		},
		"required": []string{"content"},
	}
}

func (t *TelegramMessageTool) SetContext(_ string, chatID string) {
	t.defaultChatID = chatID
	t.sentInRound = false
}

func (t *TelegramMessageTool) HasSentInRound() bool {
	return t.sentInRound
}

func (t *TelegramMessageTool) SetSendCallback(callback TelegramSendCallback) {
	t.sendCallback = callback
}

func (t *TelegramMessageTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	_ = ctx

	content, ok := args["content"].(string)
	if !ok {
		return &ToolResult{ForLLM: "content is required", IsError: true}
	}

	chatID, _ := args["chat_id"].(string)
	if chatID == "" {
		chatID = t.defaultChatID
	}
	if chatID == "" {
		return &ToolResult{ForLLM: "No target telegram chat_id specified", IsError: true}
	}

	attachmentPath, _ := args["attachment_path"].(string)

	if t.sendCallback == nil {
		return &ToolResult{ForLLM: "Telegram message sending not configured", IsError: true}
	}

	if err := t.sendCallback(chatID, content, attachmentPath); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("sending telegram message: %v", err),
			IsError: true,
			Err:     err,
		}
	}

	t.sentInRound = true
	forLLM := fmt.Sprintf("Telegram message sent to %s", chatID)
	if attachmentPath != "" {
		forLLM += " with attachment"
	}

	return &ToolResult{
		ForLLM: forLLM,
		Silent: true,
	}
}
