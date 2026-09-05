package quality

import (
	"context"
	"errors"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func TestParseVerdict(t *testing.T) {
	cases := map[string]struct {
		raw     string
		want    float64
		wantErr bool
	}{
		"clean json":                   {`{"score": 4, "reason": "correct"}`, 4, false},
		"json wrapped in prose":        {"Here is my rating:\n{\"score\": 2.5, \"reason\": \"partly wrong\"}\nThanks", 2.5, false},
		"bare score in prose":          {"Overall I would rate this a 5 out of 5.", 5, false},
		"out of five shorthand":        {"Solid answer. 4/5.", 4, false},
		"score above range is clamped": {`{"score": 9}`, 5, false},
		"score below range is clamped": {`{"score": 0}`, 1, false},
		"no number at all":             {"This response was acceptable.", 0, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := parseVerdict(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVerdict: %v", err)
			}
			if v.Score != tc.want {
				t.Fatalf("score = %v, want %v", v.Score, tc.want)
			}
		})
	}
}

type fakeProvider struct {
	provider.Provider
	reply string
	err   error
}

func (f fakeProvider) Complete(context.Context, provider.Request) (*provider.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &provider.Response{Content: f.reply}, nil
}

func TestJudgeScore(t *testing.T) {
	sample := Sample{
		RequestID: "req-1",
		Prompt:    []provider.Message{{Role: provider.RoleUser, Content: "2+2?"}},
		Response:  "4",
	}

	j := NewJudge(func(string) (provider.Provider, error) {
		return fakeProvider{reply: `{"score": 5, "reason": "correct"}`}, nil
	}, "judge-model", 0)

	v, err := j.Score(context.Background(), sample)
	if err != nil || v.Score != 5 {
		t.Fatalf("Score() = (%+v, %v)", v, err)
	}

	j = NewJudge(func(string) (provider.Provider, error) {
		return fakeProvider{err: errors.New("model down")}, nil
	}, "judge-model", 0)
	if _, err := j.Score(context.Background(), sample); err == nil {
		t.Fatal("a failed judge call must return an error, not a silent zero score")
	}
}
