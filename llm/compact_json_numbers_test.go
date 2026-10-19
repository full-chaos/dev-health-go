package llm

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCompactJSONKeepsNumberText(t *testing.T) {
	for in, want := range map[string]string{
		`{"id":9007199254740993}`:                    `{"id":9007199254740993}`,
		`{"price": 1.50}`:                            `{"price":1.50}`,
		`[ -0 , 1E2, 0.10 ]`:                         `[-0,1E2,0.10]`,
		`{"b":{"n":18446744073709551617},"a":[2.0]}`: `{"a":[2.0],"b":{"n":18446744073709551617}}`,
		`{"a":1} {"b":2}`:                            "",
		`{"a":1} x`:                                  "",
		`01`:                                         "",
	} {
		if got := CompactJSON(in); got != want {
			t.Errorf("CompactJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompactJSONPassesAnOutOfRangeNumberAsText(t *testing.T) {
	const in = `{"n":1e400}`
	got := CompactJSON(in)
	if got != in {
		t.Fatalf("CompactJSON(%q) = %q, want the text kept", in, got)
	}
	var decoded struct{ N float64 }
	if err := json.Unmarshal([]byte(got), &decoded); err == nil {
		t.Fatal("a float64 decode of 1e400 succeeded; consumers would overflow silently")
	}
}

func TestOpenAIKeepsNumberTextOfTheReply(t *testing.T) {
	const reply = `{"id":9007199254740993,"price":1.50}`
	provider, _ := newTestOpenAI(t, step{status: 200, body: openAIOK(reply)})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != reply {
		t.Errorf("Text = %q, want %q", result.Text, reply)
	}
}

func TestAnthropicKeepsNumberTextOfTheReply(t *testing.T) {
	const reply = `{"id":9007199254740993,"price":1.50}`
	provider, _ := newTestAnthropic(t, step{status: 200, body: anthropicText(reply, "end_turn")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != reply {
		t.Errorf("Text = %q, want %q", result.Text, reply)
	}
}

func TestAnthropicToolInputKeepsNumberText(t *testing.T) {
	const reply = `{"id":9007199254740993,"price":1.50}`
	provider, _ := newTestAnthropic(t, step{status: 200, body: anthropicToolUse("categorization", reply, "tool_use")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: mapFormat})
	if err != nil {
		t.Fatal(err)
	}
	if result.FormatPath != FormatPathTool || result.Text != reply {
		t.Errorf("path = %q Text = %q, want the tool path and %q", result.FormatPath, result.Text, reply)
	}
}

func TestCompatibleKeepsNumberTextOfTheReply(t *testing.T) {
	const reply = `{"id":9007199254740993,"price":1.50}`
	for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
		t.Run(string(kind), func(t *testing.T) {
			provider, _ := newTestCompatible(t, kind, step{status: 200, body: chatOK(reply)})
			result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != reply {
				t.Errorf("Text = %q, want %q", result.Text, reply)
			}
		})
	}
}
