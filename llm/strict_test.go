package llm

import (
	"context"
	"errors"
	"testing"
)

type scriptedProvider struct {
	results  []Result
	errs     []error
	requests []Request
}

func (p *scriptedProvider) Complete(_ context.Context, request Request) (Result, error) {
	i := len(p.requests)
	p.requests = append(p.requests, request)
	var err error
	if i < len(p.errs) {
		err = p.errs[i]
	}
	return p.results[min(i, len(p.results)-1)], err
}
func (p *scriptedProvider) Model() string { return "m" }
func (p *scriptedProvider) Kind() Kind    { return "scripted" }
func (p *scriptedProvider) Close() error  { return nil }

func TestCompleteStrictMakesOneRepairThenReportsInvalid(t *testing.T) {
	errMissing := errors.New("missing field a")
	validate := func(text string) error {
		if text == `{"a":1}` {
			return nil
		}
		return errMissing
	}
	var repairedFrom string
	var repairedWhy error
	repair := func(rejected string, reason error) Request {
		repairedFrom, repairedWhy = rejected, reason
		return Request{Prompt: "repair " + rejected}
	}
	cases := []struct {
		name      string
		results   []Result
		want      Outcome
		wantCalls int
		wantWhy   error
	}{
		{"first answer valid", []Result{{Text: `{"a":1}`}}, OutcomeOK, 1, nil},
		{"repair succeeds", []Result{{Text: `{"b":1}`}, {Text: `{"a":1}`}}, OutcomeRepaired, 2, errMissing},
		{"empty then invalid", []Result{{Text: ""}, {Text: `{"b":2}`}}, OutcomeInvalid, 2, ErrEmptyAnswer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repairedFrom, repairedWhy = "", nil
			provider := &scriptedProvider{results: tc.results}
			_, outcome, calls, err := CompleteStrict(context.Background(), provider, Request{Prompt: "p"}, validate, repair)
			if err != nil || outcome != tc.want || len(calls) != tc.wantCalls || len(provider.requests) != tc.wantCalls {
				t.Fatalf("outcome=%q calls=%d sent=%d err=%v", outcome, len(calls), len(provider.requests), err)
			}
			if tc.wantCalls == 2 && (repairedFrom != tc.results[0].Text || !errors.Is(repairedWhy, tc.wantWhy)) {
				t.Errorf("repair built from %q / %v", repairedFrom, repairedWhy)
			}
		})
	}
}

func TestCompleteStrictStopsOnAProviderError(t *testing.T) {
	boom := errors.New("boom")
	provider := &scriptedProvider{results: []Result{{}}, errs: []error{boom}}
	_, outcome, _, err := CompleteStrict(context.Background(), provider, Request{}, func(string) error { return nil },
		func(string, error) Request { t.Fatal("repair after a provider error"); return Request{} })
	if !errors.Is(err, boom) || outcome != "" {
		t.Fatalf("err = %v outcome = %q", err, outcome)
	}
}

func TestCompactJSON(t *testing.T) {
	for in, want := range map[string]string{` { "a" : 1 } `: `{"a":1}`, "": "", "  ": "", "not json": "", `[1,2]`: `[1,2]`, `{"a":1} extra`: ""} {
		if got := CompactJSON(in); got != want {
			t.Errorf("CompactJSON(%q) = %q, want %q", in, got, want)
		}
	}
}
