package llm

import (
	"context"
	"reflect"
	"testing"
)

var objectFormat = &JSONFormat{Type: FormatTypeJSONObject}

// With no Format, a local server is asked for no format (its modes are
// unknown; an LM Studio-style server answers 400 to json_object), while
// Gemini and Qwen, whose APIs document json_object, are asked for it.
func TestChatCompletionsWithNoFormat(t *testing.T) {
	for _, c := range []struct {
		kind Kind
		want any
	}{
		{KindLocal, nil},
		{KindGemini, map[string]any{"type": "json_object"}},
		{KindQwen, map[string]any{"type": "json_object"}},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			provider, capture := newTestCompatible(t, c.kind, step{status: 200, body: chatOK("words as written")})
			result, err := provider.Complete(context.Background(), Request{Prompt: "p"})
			if err != nil || result.Text != "words as written" || result.FormatPath != "" {
				t.Fatalf("result = %+v err = %v", result, err)
			}
			format, named := capture.request(0)["response_format"]
			if c.want == nil && named {
				t.Fatalf("response_format = %v, want none for a local server", format)
			}
			if c.want != nil && !reflect.DeepEqual(format, c.want) {
				t.Fatalf("response_format = %v, want %v", format, c.want)
			}
		})
	}
}

// A local server that refuses response_format serves a request with no
// Format: the request names none.
func TestLocalServerWithoutJSONModesServesAFormatFreeRequest(t *testing.T) {
	provider, capture := newTestCompatible(t, KindLocal, step{status: 200, body: chatOK(`{"summary":"x"}`)})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, named := capture.request(0)["response_format"]; named || capture.calls() != 1 {
		t.Fatalf("calls %d, request %v: want one call naming no response_format", capture.calls(), capture.request(0))
	}
}

// An explicit json_object Format asks every Chat Completions kind for a JSON
// object and gates the answer as JSON.
func TestJSONObjectFormatOnEveryChatCompletionsKind(t *testing.T) {
	for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
		t.Run(string(kind), func(t *testing.T) {
			provider, capture := newTestCompatible(t, kind, step{status: 200, body: chatOK(`{"a": 1}`)}, step{status: 200, body: chatOK("not json")})
			result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: objectFormat})
			if err != nil || result.Text != `{"a":1}` || result.FormatPath != FormatPathObject {
				t.Fatalf("result = %+v err = %v", result, err)
			}
			if format := capture.request(0)["response_format"]; !reflect.DeepEqual(format, map[string]any{"type": "json_object"}) {
				t.Fatalf("response_format = %v, want json_object", format)
			}
			result, err = provider.Complete(context.Background(), Request{Prompt: "p", Format: objectFormat})
			if err != nil || result.Text != "" || result.Unparsed != "not json" {
				t.Fatalf("a non-JSON answer under json_object: result = %+v err = %v, want Text \"\" and Unparsed", result, err)
			}
		})
	}
}

// The OpenAI client sends json_object for an object Format, as for none.
func TestJSONObjectFormatOnOpenAI(t *testing.T) {
	provider, capture := newTestOpenAI(t, step{status: 200, body: openAIOK(`{"a": 1}`)})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: objectFormat})
	if err != nil || result.Text != `{"a":1}` || result.FormatPath != FormatPathObject {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	text, _ := capture.request(0)["text"].(map[string]any)
	if format := text["format"]; !reflect.DeepEqual(format, map[string]any{"type": "json_object"}) {
		t.Fatalf("text.format = %v, want json_object", format)
	}
}

// The Messages API has no json_object mode: an object Format sends no
// output_config and no tool, and the answer is gated as JSON.
func TestJSONObjectFormatOnAnthropic(t *testing.T) {
	provider, capture := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"a": 1}`, "end_turn")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: objectFormat})
	if err != nil || result.Text != `{"a":1}` || result.FormatPath != FormatPathObject {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	body := capture.request(0)
	if body["output_config"] != nil || body["tools"] != nil || body["tool_choice"] != nil {
		t.Fatalf("request = %v, want no output_config, tools or tool_choice", body)
	}
}
