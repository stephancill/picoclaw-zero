package channels

import (
	"testing"

	"github.com/mymmrac/telego"
)

func TestBuildTelegramReplyPreview_Text(t *testing.T) {
	preview := buildTelegramReplyPreview(&telego.Message{Text: "Original message"})
	if preview != "Original message" {
		t.Fatalf("preview = %q, want %q", preview, "Original message")
	}
}

func TestBuildTelegramReplyPreview_MediaFallback(t *testing.T) {
	preview := buildTelegramReplyPreview(&telego.Message{Document: &telego.Document{}})
	if preview != "[file]" {
		t.Fatalf("preview = %q, want %q", preview, "[file]")
	}
}
