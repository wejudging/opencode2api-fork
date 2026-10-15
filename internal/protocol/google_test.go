package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConvertChatToGoogleRequest(t *testing.T) {
	chat := map[string]any{
		"model":      "gemini-4-argon-contributor",
		"stream":     false,
		"max_tokens": 128,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are terse."},
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "lookup",
			"description": "look up a value",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		}}},
	}
	converted, err := ConvertRequest(Chat, Google, chat)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if converted["model"] != "gemini-4-argon-contributor" {
		t.Fatalf("model not carried: %v", converted["model"])
	}
	if _, ok := converted["stream"]; !ok {
		t.Fatalf("stream not carried")
	}
	system := jsonMap(converted, "systemInstruction")
	if got := jsonText(system, "parts"); got != "You are terse." {
		t.Fatalf("system instruction = %q", got)
	}
	contents := converted["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len = %d", len(contents))
	}
	content := contents[0].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) == 0 {
		t.Fatalf("parts missing")
	}
	if part := parts[0].(map[string]any); part["text"] != "hi" {
		t.Fatalf("user text = %v", part["text"])
	}
	if !strings.Contains(mustJSON(t, converted), "maxOutputTokens") {
		t.Fatalf("maxOutputTokens missing: %s", mustJSON(t, converted))
	}
	if !strings.Contains(mustJSON(t, converted), "functionDeclarations") {
		t.Fatalf("functionDeclarations missing: %s", mustJSON(t, converted))
	}
}

func TestConvertGoogleToChatResponse(t *testing.T) {
	body := []byte(`{"responseId":"abc","modelVersion":"gemini-4-argon-contributor","candidates":[{"finishReason":"STOP","index":0,"content":{"role":"model","parts":[{"text":"Hi."}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}}`)
	converted, err := ConvertResponse(Google, Chat, body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(converted, &decoded); err != nil {
		t.Fatal(err)
	}
	choices := decoded["choices"].([]any)
	message := jsonMap(choices[0].(map[string]any), "message")
	if message["content"] != "Hi." {
		t.Fatalf("content = %v", message["content"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
}

func TestParseGoogleStream(t *testing.T) {
	parser := &bridgeStreamParser{
		protocol: Google,
		tools:    map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}
	var text strings.Builder
	sawDone := false
	for _, line := range []string{
		`{"responseId":"abc","modelVersion":"m","candidates":[{"index":0,"content":{"parts":[{"text":"1, 2"}]}}]}`,
		`{"responseId":"abc","candidates":[{"finishReason":"STOP","index":0,"content":{"parts":[{"text":""}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`,
	} {
		events, err := parser.Parse("", line)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		for _, event := range events {
			if event.Kind == "text" {
				text.WriteString(event.Text)
			}
			if event.Kind == "done" {
				sawDone = true
			}
		}
	}
	if text.String() != "1, 2" {
		t.Fatalf("text = %q", text.String())
	}
	if !sawDone {
		t.Fatalf("stream did not terminate")
	}
}

func jsonMap(object map[string]any, key string) map[string]any {
	value, _ := object[key].(map[string]any)
	return value
}

func jsonText(object map[string]any, key string) string {
	parts, _ := object[key].([]any)
	if len(parts) == 0 {
		return ""
	}
	part, _ := parts[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
