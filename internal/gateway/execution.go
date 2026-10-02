package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type OpenAIRoute struct {
	Upstream *url.URL
	Creds    CredentialStore
}

// maxModelRequest bounds one decompressed model request. Agent contexts with
// inline images run to a few MB; the vendors' own limits sit around 32 MB.
const maxModelRequest = 32 << 20

type executionPolicy interface {
	ExecutionPolicy(space string) (route string, internet bool, err error)
}

// prepareRequest enforces the host's account choice and rejects provider-side
// network tools when Internet access is off. Local function tools remain usable.
func prepareRequest(r *http.Request, provider, route string, internet bool) (*http.Request, error) {
	if r.Method != http.MethodPost {
		if !internet {
			return nil, fmt.Errorf("Internet access is off: only model requests are allowed")
		}
		return r, nil
	}
	chatCompletions := strings.HasSuffix(r.URL.Path, "/chat/completions")
	if internet && !chatCompletions && (provider != "openai" || (r.URL.Path != "/openai/responses" && r.URL.Path != "/openai/responses/compact")) {
		return r, nil
	}
	if !internet {
		allowed := map[string]bool{"/openai/responses": true, "/openai/responses/compact": true, "/openai/chat/completions": true, "/anthropic/v1/messages": true, "/anthropic/v1/messages/count_tokens": true, "/xai/responses": true, "/xai/chat/completions": true}
		if !allowed[r.URL.Path] {
			return nil, fmt.Errorf("Internet access is off: endpoint is unavailable")
		}
	}
	// Limit decompressed input too: requests may carry inline image data.
	var reader io.Reader = r.Body
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("invalid compressed request")
		}
		defer gz.Close()
		reader = gz
	case "zstd":
		z, err := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			return nil, fmt.Errorf("invalid compressed request")
		}
		defer z.Close()
		reader = z
	default:
		return nil, fmt.Errorf("unsupported request encoding")
	}
	b, err := io.ReadAll(io.LimitReader(reader, maxModelRequest+1))
	if err != nil || len(b) > maxModelRequest {
		return nil, fmt.Errorf("model request is too large or unreadable")
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(b, &payload) != nil || payload == nil {
		return nil, fmt.Errorf("invalid model request JSON")
	}
	if chatCompletions {
		// A streamed chat completion carries no usage block unless asked for,
		// which would leave the request unmetered. Ask for it; the extra final
		// chunk is part of the public API and clients ignore what they don't use.
		var stream bool
		if raw, ok := payload["stream"]; ok && json.Unmarshal(raw, &stream) == nil && stream {
			var opts map[string]json.RawMessage
			if raw, ok := payload["stream_options"]; ok {
				_ = json.Unmarshal(raw, &opts)
			}
			if opts == nil {
				opts = map[string]json.RawMessage{}
			}
			if _, ok := opts["include_usage"]; !ok {
				opts["include_usage"] = json.RawMessage("true")
				payload["stream_options"], _ = json.Marshal(opts)
			}
		}
	}
	if provider == "openai" {
		var model string
		if raw, ok := payload["model"]; ok {
			if json.Unmarshal(raw, &model) != nil || model == "" {
				return nil, fmt.Errorf("invalid model")
			}
			// Routing is selected by the host, never by a guest-controlled prefix.
			for _, prefix := range []string{"sub2api-i/", "sub2api-ii/", "sub2api/"} {
				model = strings.TrimPrefix(model, prefix)
			}
			if strings.Contains(model, "/") {
				return nil, fmt.Errorf("invalid model route prefix")
			}
			payload["model"], _ = json.Marshal(model)
		}
	}
	if !internet {
		var document any
		if json.Unmarshal(b, &document) != nil {
			return nil, fmt.Errorf("invalid model request")
		}
		if err := rejectRemoteContent(document); err != nil {
			return nil, err
		}
		var tools []map[string]any
		if raw, ok := payload["tools"]; ok && json.Unmarshal(raw, &tools) != nil {
			return nil, fmt.Errorf("invalid tools")
		}
		for _, tool := range tools {
			if !localTool(tool) {
				return nil, fmt.Errorf("Internet access is off: hosted tool %q is unavailable", tool["type"])
			}
		}
		for _, key := range []string{"search_parameters", "web_search_options", "mcp_servers", "container", "previous_response_id"} {
			if v, ok := payload[key]; ok && string(v) != "null" {
				return nil, fmt.Errorf("Internet access is off: %s is unavailable", key)
			}
		}
	}
	b, err = json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	out := r.Clone(r.Context())
	out.Body = io.NopCloser(bytes.NewReader(b))
	out.ContentLength = int64(len(b))
	out.Header = r.Header.Clone()
	out.Header.Del("Content-Encoding")
	out.Header.Del("Content-Length")
	if provider == "openai" && route != "official" {
		out.Header.Del("Chatgpt-Account-Id")
		out.Header.Del("OpenAI-Organization")
		out.Header.Del("OpenAI-Project")
	}
	return out, nil
}

func localTool(tool map[string]any) bool {
	switch tool["type"] {
	case "function", "custom", "local_shell", "apply_patch":
		return true
	case "shell":
		env, _ := tool["environment"].(map[string]any)
		return env["type"] == "local"
	case "namespace":
		children, ok := tool["tools"].([]any)
		if !ok {
			return false
		}
		for _, child := range children {
			c, ok := child.(map[string]any)
			if !ok || !localTool(c) {
				return false
			}
		}
		return true
	case nil:
		// Anthropic custom local tools omit type and supply an input_schema.
		_, schema := tool["input_schema"]
		_, name := tool["name"]
		return schema && name
	}
	return false
}

// mediaBlockTypes are the content-block types whose "url" field makes the
// provider fetch something remotely. Only those are rejected offline. A URL
// that merely appears as data — the arguments of a local tool call the
// assistant made earlier, a tool result, free text — is not a fetch, and
// rejecting it would poison the whole conversation history after a single
// WebFetch attempt.
var mediaBlockTypes = map[string]bool{
	"image": true, "document": true, "input_image": true, "input_file": true,
	"file": true, "url": true, "image_url": true, "audio": true, "input_audio": true, "video": true,
}

func remoteURL(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	l := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(l, "http:") || strings.HasPrefix(l, "https:")
}

func rejectRemoteContent(value any) error {
	switch v := value.(type) {
	case map[string]any:
		typ, _ := v["type"].(string)
		for k, child := range v {
			switch k {
			case "server_url":
				if remoteURL(child) {
					return fmt.Errorf("Internet access is off: remote MCP servers are unavailable")
				}
			case "url", "file_url":
				if remoteURL(child) && mediaBlockTypes[typ] {
					return fmt.Errorf("Internet access is off: remote media URLs are unavailable")
				}
			case "image_url":
				// OpenAI: "image_url": "https://…" or "image_url": {"url": "https://…"}
				if remoteURL(child) {
					return fmt.Errorf("Internet access is off: remote media URLs are unavailable")
				}
				if m, ok := child.(map[string]any); ok && remoteURL(m["url"]) {
					return fmt.Errorf("Internet access is off: remote media URLs are unavailable")
				}
			}
			// Schemas describe local code; a typed block's input/arguments/
			// output (tool_use, function_call, function_call_output) is data
			// the model produced or received, not an instruction to fetch.
			// The request's own top-level "input" (Responses API) has no
			// type and is the message list, so it is still walked.
			switch k {
			case "parameters", "input_schema":
				continue
			case "input", "arguments", "output":
				if typ != "" {
					continue
				}
			}
			if err := rejectRemoteContent(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := rejectRemoteContent(child); err != nil {
				return err
			}
		}
	}
	return nil
}
