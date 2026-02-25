package tools

import (
	"context"
	"errors"
	"testing"
)

func TestTelegramMessageTool_Execute_SuccessWithAttachment(t *testing.T) {
	tool := NewTelegramMessageTool()
	tool.SetContext("telegram", "12345")

	var sentChatID, sentContent, sentAttachment string
	tool.SetSendCallback(func(chatID, content, attachmentPath string) error {
		sentChatID = chatID
		sentContent = content
		sentAttachment = attachmentPath
		return nil
	})

	result := tool.Execute(context.Background(), map[string]interface{}{
		"content":         "Hello",
		"attachment_path": "/tmp/file.txt",
	})

	if sentChatID != "12345" {
		t.Errorf("Expected chatID '12345', got '%s'", sentChatID)
	}
	if sentContent != "Hello" {
		t.Errorf("Expected content 'Hello', got '%s'", sentContent)
	}
	if sentAttachment != "/tmp/file.txt" {
		t.Errorf("Expected attachment '/tmp/file.txt', got '%s'", sentAttachment)
	}
	if result.IsError {
		t.Fatal("Expected success result")
	}
	if !result.Silent {
		t.Fatal("Expected Silent=true")
	}
}

func TestTelegramMessageTool_Execute_Failure(t *testing.T) {
	tool := NewTelegramMessageTool()
	tool.SetContext("telegram", "12345")

	sendErr := errors.New("failed")
	tool.SetSendCallback(func(chatID, content, attachmentPath string) error {
		return sendErr
	})

	result := tool.Execute(context.Background(), map[string]interface{}{
		"content": "Hello",
	})

	if !result.IsError {
		t.Fatal("Expected error result")
	}
	if result.Err != sendErr {
		t.Fatalf("Expected send error, got %v", result.Err)
	}
}

func TestTelegramMessageTool_Execute_UsesCustomChatID(t *testing.T) {
	tool := NewTelegramMessageTool()
	tool.SetContext("telegram", "default")

	var sentChatID string
	tool.SetSendCallback(func(chatID, content, attachmentPath string) error {
		sentChatID = chatID
		return nil
	})

	result := tool.Execute(context.Background(), map[string]interface{}{
		"content": "Hello",
		"chat_id": "custom",
	})

	if result.IsError {
		t.Fatal("Expected success result")
	}
	if sentChatID != "custom" {
		t.Fatalf("Expected custom chatID, got '%s'", sentChatID)
	}
}
