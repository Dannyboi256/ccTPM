package parser

import (
	"strings"
	"testing"
)

func TestParseJSON(t *testing.T) {
	body := `{
		"id": "msg_123",
		"type": "message",
		"model": "claude-sonnet-4-20250514",
		"usage": {
			"input_tokens": 1234,
			"output_tokens": 567,
			"cache_creation_input_tokens": 100,
			"cache_read_input_tokens": 800
		}
	}`
	result, err := ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON failed: %v", err)
	}
	if result.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %s", result.Model)
	}
	if result.InputTokens != 1234 {
		t.Fatalf("expected 1234 input tokens, got %d", result.InputTokens)
	}
	if result.OutputTokens != 567 {
		t.Fatalf("expected 567 output tokens, got %d", result.OutputTokens)
	}
	if result.CacheCreation != 100 {
		t.Fatalf("expected 100 cache creation, got %d", result.CacheCreation)
	}
	if result.CacheRead != 800 {
		t.Fatalf("expected 800 cache read, got %d", result.CacheRead)
	}
}

func TestParseJSONIgnoresUnknownFields(t *testing.T) {
	body := `{"model": "x", "usage": {"input_tokens": 1, "output_tokens": 2, "server_tool_use": {"web_search_requests": 1}}, "unknown_field": true}`
	result, err := ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON failed on unknown fields: %v", err)
	}
	if result.InputTokens != 1 {
		t.Fatalf("expected 1, got %d", result.InputTokens)
	}
}

func TestParseSSEBasic(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":1234,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":800}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":567}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseSSE failed: %v", err)
	}
	if result.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %s", result.Model)
	}
	if result.InputTokens != 1234 {
		t.Fatalf("expected 1234 input, got %d", result.InputTokens)
	}
	if result.OutputTokens != 567 {
		t.Fatalf("expected 567 output, got %d", result.OutputTokens)
	}
	if result.CacheRead != 800 {
		t.Fatalf("expected 800 cache read, got %d", result.CacheRead)
	}
}

func TestParseSSEMessageDeltaOverridesInputTokens(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":5000,\"output_tokens\":200}}\n\n"
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != 5000 {
		t.Fatalf("expected message_delta to override input_tokens to 5000, got %d", result.InputTokens)
	}
	if result.OutputTokens != 200 {
		t.Fatalf("expected 200 output, got %d", result.OutputTokens)
	}
}

func TestParseSSEMissingUsageInDelta(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}}\n\n"
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != 100 {
		t.Fatalf("expected 100 input, got %d", result.InputTokens)
	}
	if result.OutputTokens != 1 {
		t.Fatalf("expected 1 output, got %d", result.OutputTokens)
	}
}

func TestParseSSEErrorEvent(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	result, err := ParseSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasError {
		t.Fatal("expected HasError=true for error event")
	}
}
