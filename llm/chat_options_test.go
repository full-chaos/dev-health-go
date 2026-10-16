package llm

import (
	"context"
	"strings"
	"testing"
)

func TestCompatibleTemperatureModes(t *testing.T) {
	cases := []struct {
		name    string
		mode    TemperatureMode
		want    any
		present bool
	}{
		{name: "the zero value sends 0.3", mode: TemperatureDefault, want: 0.3, present: true},
		{name: "omit sends no temperature", mode: TemperatureOmit, present: false},
	}
	for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				server, c := scriptedServer(t, step{status: 200, body: chatOK(`{"a":1}`)})
				provider, err := NewCompatible(Config{Kind: kind, APIKey: NewSecret(testKey), BaseURL: server.URL, Model: "m", Temperature: tc.mode})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
					t.Fatal(err)
				}
				got, present := c.request(0)["temperature"]
				if present != tc.present || (tc.present && got != tc.want) {
					t.Errorf("temperature = %v (present %v), want %v (present %v)", got, present, tc.want, tc.present)
				}
			})
		}
	}
}

func TestCompatibleSendsTheSeedOnEveryAttempt(t *testing.T) {
	seed := int64(9007199254740993)
	for _, kind := range []Kind{KindLocal, KindQwen} {
		t.Run(string(kind), func(t *testing.T) {
			provider, c := newTestCompatible(t, kind,
				step{status: 400, body: `{"error":{"message":"response_format json_schema is not supported"}}`},
				step{status: 200, body: chatOK(`{"a":2}`)},
			)
			if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat, Seed: &seed}); err != nil {
				t.Fatal(err)
			}
			if c.calls() != 2 {
				t.Fatalf("calls = %d, want 2", c.calls())
			}
			for i := 0; i < c.calls(); i++ {
				if got := string(c.bodies[i]); !strings.Contains(got, `"seed":9007199254740993`) {
					t.Errorf("request %d body carries no exact seed: %s", i, got)
				}
			}
		})
	}
}

func TestCompatibleSendsNoSeedWhenNoneIsAsked(t *testing.T) {
	provider, c := newTestCompatible(t, KindQwen, step{status: 200, body: chatOK(`{"a":1}`)})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if _, present := c.request(0)["seed"]; present {
		t.Errorf("seed sent with none asked: %v", c.request(0)["seed"])
	}
}

func TestResponsesMessagesAndGeminiRefuseASeedBeforeAnyCall(t *testing.T) {
	seed := int64(7)
	openAI, openAICalls := newTestOpenAI(t, step{status: 200, body: openAIOK(`{"a":1}`)})
	anthropic, anthropicCalls := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"a":1}`, "end_turn")})
	gemini, geminiCalls := newTestCompatible(t, KindGemini, step{status: 200, body: chatOK(`{"a":1}`)})
	for _, tc := range []struct {
		provider Provider
		calls    *capture
	}{{openAI, openAICalls}, {anthropic, anthropicCalls}, {gemini, geminiCalls}} {
		t.Run(string(tc.provider.Kind()), func(t *testing.T) {
			_, err := tc.provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat, Seed: &seed})
			if class, ok := ClassOf(err); !ok || class != ClassInvalidRequest {
				t.Fatalf("err = %v, want class %q", err, ClassInvalidRequest)
			}
			if tc.calls.calls() != 0 {
				t.Errorf("calls = %d, want none", tc.calls.calls())
			}
		})
	}
}

func TestUnparsedIsEmptyForAValidOrSchemaFreeReply(t *testing.T) {
	provider, _ := newTestCompatible(t, KindLocal, step{status: 200, body: chatOK(`{"a":1}`)}, step{status: 200, body: chatOK("plain words")})
	valid, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || valid.Unparsed != "" {
		t.Fatalf("valid reply: %+v %v", valid, err)
	}
	free, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if err != nil || free.Text != "plain words" || free.Unparsed != "" {
		t.Fatalf("schema-free reply: %+v %v", free, err)
	}
}

func TestAnthropicRefusalLeavesUnparsedEmpty(t *testing.T) {
	provider, _ := newTestAnthropic(t, step{status: 200, body: anthropicText("I will not answer that.", "refusal")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != "" || result.Unparsed != "" || result.StopReason != "refusal" {
		t.Fatalf("result = %+v err = %v", result, err)
	}
}

func TestErrorStatusCodeIsTheResponseStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{429, `{"error":{"message":"You exceeded your current quota","code":"insufficient_quota"}}`},
		{429, `{"error":{"message":"Rate limit reached"}}`},
		{401, `{"error":{"message":"bad key"}}`},
		{503, `{"error":{"message":"overloaded"}}`},
	} {
		provider, _ := newTestCompatible(t, KindLocal, step{status: tc.status, body: tc.body})
		_, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
		var typed *Error
		if !asError(err, &typed) || typed.StatusCode() != tc.status {
			t.Errorf("status %d: err = %v, StatusCode = %d", tc.status, err, statusOrZero(typed))
		}
	}
	seed := int64(1)
	openAI, _ := newTestOpenAI(t, step{status: 200, body: openAIOK(`{"a":1}`)})
	_, err := openAI.Complete(context.Background(), Request{Prompt: "p", Seed: &seed})
	var refused *Error
	if !asError(err, &refused) || refused.StatusCode() != 0 {
		t.Errorf("a refused seed: err = %v, StatusCode = %d, want 0", err, statusOrZero(refused))
	}
}

func statusOrZero(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}
