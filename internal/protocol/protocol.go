// Package protocol translates Chat, Responses, and Anthropic payloads and
// streams, and carries the opaque System One decision payloads.
package protocol

type Protocol string

const (
	Chat      Protocol = "chat"
	Responses Protocol = "responses"
	Anthropic Protocol = "anthropic"
	// SystemOne is OpenCode Zen's structured decision endpoint. A request pairs
	// a free-form state with typed questions and the reply carries typed
	// answers, so the payload shares no shape with the chat/responses bridge and
	// is forwarded as-is.
	SystemOne Protocol = "systemone"
	// Google is the native generateContent protocol used by OpenCode's Go-tier
	// Gemini models. It has no fixed upstream path (the model id is part of the
	// URL and streaming uses a different verb), so Path cannot describe it;
	// callers that build the endpoint must special-case it. It is never exposed
	// as a client-facing wire protocol: the bridge translates it to and from
	// Chat.
	Google Protocol = "google"
)

func Valid(p Protocol) bool {
	return p == Chat || p == Responses || p == Anthropic || p == SystemOne || p == Google
}

func Path(protocol Protocol) string {
	switch protocol {
	case Responses:
		return "/v1/responses"
	case Anthropic:
		return "/v1/messages"
	case SystemOne:
		return "/v1/systemone"
	default:
		// Google is internal-only and its endpoint is model-and-stream
		// dependent; fall back to the Chat path, which the debug UI uses to
		// reach the locally exposed route.
		return "/v1/chat/completions"
	}
}
