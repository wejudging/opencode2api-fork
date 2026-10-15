package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
)

// Google is the native generateContent protocol used by OpenCode's Go-tier
// Gemini models (for example gemini-4-argon-contributor). Unlike the other
// protocols it is provider-shaped rather than message-shaped, so the bridge
// translates it to and from the Chat representation the gateway exposes.
func encodeGoogleRequest(request bridgeRequest) (map[string]any, error) {
	output := map[string]any{"model": request.Model, "stream": request.Stream}
	system := append(append([]bridgeBlock{}, request.System...), request.Developer...)
	if text := strings.TrimSpace(bridgeBlocksText(system)); text != "" {
		output["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": text}}}
	}
	toolNames := make(map[string]string)
	for _, message := range request.Messages {
		for _, block := range message.Blocks {
			if block.Kind == "tool_call" && block.ID != "" {
				toolNames[block.ID] = block.Name
			}
		}
	}
	contents := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		parts := make([]any, 0, len(message.Blocks))
		for _, block := range message.Blocks {
			switch block.Kind {
			case "text":
				if block.Text != "" {
					parts = append(parts, map[string]any{"text": block.Text})
				}
			case "image", "file":
				if part := googleInlinePart(block); part != nil {
					parts = append(parts, part)
				}
			case "tool_call":
				parts = append(parts, map[string]any{"functionCall": map[string]any{
					"name": block.Name,
					"args": jsonutil.ParseJSONOrString(block.ArgumentsJSON),
				}})
			case "tool_result":
				name := toolNames[block.CallID]
				if name == "" {
					name = block.Name
				}
				if name == "" {
					continue
				}
				parts = append(parts, map[string]any{"functionResponse": map[string]any{
					"name":     name,
					"response": googleToolResponse(block.Result),
				}})
			}
		}
		if len(parts) == 0 {
			continue
		}
		role := "user"
		if message.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("google request contains no convertible content")
	}
	output["contents"] = contents
	if len(request.Tools) > 0 {
		declarations := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			declaration := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				declaration["description"] = tool.Description
			}
			if tool.Schema != nil {
				declaration["parameters"] = tool.Schema
			}
			declarations = append(declarations, declaration)
		}
		output["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	if config := googleToolConfig(request.ToolChoice); config != nil {
		output["toolConfig"] = config
	}
	if generation := googleGenerationConfig(request); len(generation) > 0 {
		output["generationConfig"] = generation
	}
	return output, nil
}

func googleGenerationConfig(request bridgeRequest) map[string]any {
	generation := map[string]any{}
	jsonutil.Put(generation, "temperature", request.Temperature)
	jsonutil.Put(generation, "topP", request.TopP)
	jsonutil.Put(generation, "maxOutputTokens", request.MaxTokens)
	if stops := googleStopSequences(request.Stop); len(stops) > 0 {
		generation["stopSequences"] = stops
	}
	return generation
}

func googleStopSequences(value any) []any {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []any{typed}
	case []any:
		stops := make([]any, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				stops = append(stops, text)
			}
		}
		return stops
	default:
		return nil
	}
}

func googleToolConfig(choice bridgeToolChoice) map[string]any {
	config := func(mode string) map[string]any {
		return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
	}
	switch choice.Mode {
	case "none":
		return config("NONE")
	case "required":
		return config("ANY")
	case "named":
		call := map[string]any{"mode": "ANY"}
		if choice.Name != "" {
			call["allowedFunctionNames"] = []any{choice.Name}
		}
		return map[string]any{"functionCallingConfig": call}
	default:
		return nil
	}
}

func googleInlinePart(block bridgeBlock) map[string]any {
	if block.Data == "" {
		return nil
	}
	mimeType := block.MediaType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return map[string]any{"inlineData": map[string]any{"mimeType": mimeType, "data": block.Data}}
}

func googleToolResponse(result any) any {
	switch value := result.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return value
	case []any:
		return map[string]any{"content": value}
	default:
		return map[string]any{"result": value}
	}
}

func googleArgumentsJSON(value any) string {
	if value == nil {
		return "{}"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func googleStop(finish string, hasTools bool) string {
	switch strings.ToUpper(strings.TrimSpace(finish)) {
	case "STOP":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL":
		return "error"
	case "":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	default:
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	}
}

func googleFinishReason(stop string) string {
	switch stop {
	case "length":
		return "MAX_TOKENS"
	case "content_filter", "error":
		return "SAFETY"
	default:
		return "STOP"
	}
}

func decodeGoogleUsage(usage map[string]any) Usage {
	input := jsonutil.IntAt(usage, "promptTokenCount")
	cached := jsonutil.IntAt(usage, "cachedContentTokenCount")
	output := jsonutil.IntAt(usage, "candidatesTokenCount")
	reasoning := jsonutil.IntAt(usage, "thoughtsTokenCount")
	total := jsonutil.IntAt(usage, "totalTokenCount")
	if total == 0 {
		total = input + output
	}
	return Usage{Input: input, Output: output, Total: total, Cached: cached, Reasoning: reasoning}
}

func decodeGoogleRequest(input map[string]any) (bridgeRequest, error) {
	request := bridgeRequest{
		Model:  jsonutil.StringAt(input, "model"),
		Stream: jsonutil.BoolAt(input, "stream"),
	}
	if generation := jsonutil.MapAt(input, "generationConfig"); len(generation) > 0 {
		request.Temperature = generation["temperature"]
		request.TopP = generation["topP"]
		request.MaxTokens = generation["maxOutputTokens"]
		if stops := jsonutil.SliceAt(generation, "stopSequences"); len(stops) > 0 {
			request.Stop = stops
		}
	}
	if system := jsonutil.MapAt(input, "systemInstruction"); len(system) > 0 {
		for _, raw := range jsonutil.SliceAt(system, "parts") {
			part, _ := raw.(map[string]any)
			if text := jsonutil.StringAt(part, "text"); text != "" {
				request.System = append(request.System, bridgeBlock{Kind: "text", Text: text})
			}
		}
	}
	for i, raw := range jsonutil.SliceAt(input, "contents") {
		content, ok := raw.(map[string]any)
		if !ok {
			return request, fmt.Errorf("contents[%d] must be an object", i)
		}
		role := "user"
		if jsonutil.StringAt(content, "role") == "model" {
			role = "assistant"
		}
		blocks := make([]bridgeBlock, 0, 2)
		for _, rawPart := range jsonutil.SliceAt(content, "parts") {
			part, _ := rawPart.(map[string]any)
			if text := jsonutil.StringAt(part, "text"); text != "" {
				blocks = append(blocks, bridgeBlock{Kind: "text", Text: text})
			}
			if call := jsonutil.MapAt(part, "functionCall"); len(call) > 0 {
				args := call["args"]
				blocks = append(blocks, bridgeBlock{
					Kind:          "tool_call",
					ID:            identity.RandomID("call", 12),
					Name:          jsonutil.StringAt(call, "name"),
					Arguments:     args,
					ArgumentsJSON: googleArgumentsJSON(args),
				})
			}
			if response := jsonutil.MapAt(part, "functionResponse"); len(response) > 0 {
				blocks = append(blocks, bridgeBlock{
					Kind:   "tool_result",
					CallID: identity.RandomID("call", 12),
					Result: response["response"],
				})
			}
		}
		if len(blocks) > 0 {
			request.Messages = append(request.Messages, bridgeMessage{Role: role, Blocks: blocks})
		}
	}
	for _, raw := range jsonutil.SliceAt(input, "tools") {
		tool, _ := raw.(map[string]any)
		for _, rawDeclaration := range jsonutil.SliceAt(tool, "functionDeclarations") {
			declaration, _ := rawDeclaration.(map[string]any)
			request.Tools = append(request.Tools, bridgeTool{
				Name:        jsonutil.StringAt(declaration, "name"),
				Description: jsonutil.StringAt(declaration, "description"),
				Schema:      declaration["parameters"],
			})
		}
	}
	if mode := jsonutil.StringAt(input, "toolConfig", "functionCallingConfig", "mode"); mode != "" {
		switch strings.ToUpper(mode) {
		case "NONE":
			request.ToolChoice = bridgeToolChoice{Mode: "none"}
		case "ANY":
			if names := jsonutil.SliceAt(input, "toolConfig", "functionCallingConfig", "allowedFunctionNames"); len(names) == 1 {
				if name, ok := names[0].(string); ok {
					request.ToolChoice = bridgeToolChoice{Mode: "named", Name: name}
					break
				}
			}
			request.ToolChoice = bridgeToolChoice{Mode: "required"}
		}
	}
	return request, nil
}

func decodeGoogleResponse(input map[string]any) (bridgeResponse, error) {
	response := bridgeResponse{
		ID:      jsonutil.FirstString(jsonutil.StringAt(input, "responseId"), jsonutil.StringAt(input, "id")),
		Model:   jsonutil.FirstString(jsonutil.StringAt(input, "modelVersion"), jsonutil.StringAt(input, "model")),
		Created: time.Now().Unix(),
	}
	if message := jsonutil.StringAt(input, "error", "message"); message != "" {
		response.Error = message
		response.Stop = "error"
		return response, nil
	}
	candidates := jsonutil.SliceAt(input, "candidates")
	if len(candidates) == 0 {
		if jsonutil.StringAt(input, "promptFeedback", "blockReason") != "" {
			response.Stop = "content_filter"
			return response, nil
		}
		return response, fmt.Errorf("google response contains no candidates")
	}
	candidate, _ := candidates[0].(map[string]any)
	content := jsonutil.MapAt(candidate, "content")
	for _, raw := range jsonutil.SliceAt(content, "parts") {
		part, _ := raw.(map[string]any)
		if text := jsonutil.StringAt(part, "text"); text != "" {
			response.Text += text
		}
		if call := jsonutil.MapAt(part, "functionCall"); len(call) > 0 {
			args := call["args"]
			response.Tools = append(response.Tools, bridgeBlock{
				Kind:          "tool_call",
				ID:            identity.RandomID("call", 12),
				Name:          jsonutil.StringAt(call, "name"),
				Arguments:     args,
				ArgumentsJSON: googleArgumentsJSON(args),
			})
		}
	}
	response.Stop = googleStop(jsonutil.StringAt(candidate, "finishReason"), len(response.Tools) > 0)
	response.Usage = decodeGoogleUsage(jsonutil.MapAt(input, "usageMetadata"))
	return response, nil
}

func encodeGoogleResponse(response bridgeResponse) map[string]any {
	parts := make([]any, 0, len(response.Tools)+1)
	if response.Text != "" {
		parts = append(parts, map[string]any{"text": response.Text})
	}
	for _, tool := range response.Tools {
		args := tool.Arguments
		if args == nil {
			args = jsonutil.ParseJSONOrString(tool.ArgumentsJSON)
		}
		parts = append(parts, map[string]any{"functionCall": map[string]any{"name": tool.Name, "args": args}})
	}
	return map[string]any{
		"responseId": asPrefix(response.ID, "resp"),
		"candidates": []any{map[string]any{
			"index":        0,
			"finishReason": googleFinishReason(response.Stop),
			"content":      map[string]any{"role": "model", "parts": parts},
		}},
		"modelVersion": response.Model,
		"usageMetadata": map[string]any{
			"promptTokenCount":        response.Usage.Input,
			"candidatesTokenCount":    response.Usage.Output,
			"totalTokenCount":         response.Usage.Total,
			"cachedContentTokenCount": response.Usage.Cached,
			"thoughtsTokenCount":      response.Usage.Reasoning,
		},
	}
}

func (parser *bridgeStreamParser) parseGoogleEvent(value map[string]any) []bridgeStreamEvent {
	events := make([]bridgeStreamEvent, 0, 4)
	if message := jsonutil.StringAt(value, "error", "message"); message != "" {
		return []bridgeStreamEvent{{Kind: "error", Error: message, ErrorType: jsonutil.FirstString(jsonutil.StringAt(value, "error", "status"), "upstream_error")}}
	}
	if !parser.started {
		responseID := jsonutil.FirstString(jsonutil.StringAt(value, "responseId"), jsonutil.StringAt(value, "id"))
		if responseID == "" {
			responseID = identity.RandomID("resp", 12)
		}
		parser.started = true
		events = append(events, bridgeStreamEvent{Kind: "start", ResponseID: responseID, Model: jsonutil.FirstString(jsonutil.StringAt(value, "modelVersion"), jsonutil.StringAt(value, "model"))})
	}
	finished := ""
	for _, raw := range jsonutil.SliceAt(value, "candidates") {
		candidate, _ := raw.(map[string]any)
		content := jsonutil.MapAt(candidate, "content")
		for i, rawPart := range jsonutil.SliceAt(content, "parts") {
			part, _ := rawPart.(map[string]any)
			if text := jsonutil.StringAt(part, "text"); text != "" {
				events = append(events, bridgeStreamEvent{Kind: "text", Text: text})
			}
			call := jsonutil.MapAt(part, "functionCall")
			if len(call) == 0 {
				continue
			}
			key := fmt.Sprint(i)
			name := jsonutil.StringAt(call, "name")
			if _, seen := parser.tools[key]; !seen {
				parser.tools[key] = true
				parser.toolOrder = append(parser.toolOrder, key)
				parser.toolIDs[key] = identity.RandomID("call", 12)
				parser.toolNames[key] = name
				events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: name})
			}
			if arguments := googleArgumentsJSON(call["args"]); arguments != "" {
				events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: name, Text: arguments})
			}
		}
		if finish := jsonutil.StringAt(candidate, "finishReason"); finish != "" {
			finished = finish
		}
	}
	if usageMap := jsonutil.MapAt(value, "usageMetadata"); len(usageMap) > 0 {
		usage := decodeGoogleUsage(usageMap)
		events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
	}
	if finished != "" {
		events = append(events, bridgeStreamEvent{Kind: "finish", Stop: googleStop(finished, len(parser.toolOrder) > 0)})
		events = append(events, bridgeStreamEvent{Kind: "done"})
	}
	return events
}
