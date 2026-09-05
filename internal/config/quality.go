package config

import (
	"fmt"
	"os"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/Harshalsharma05/switchyard/internal/quality"
)

// Quality is the validated result of loading configs/quality.yaml. It carries
// an already-built sampler config so nothing downstream re-parses this file.
type Quality struct {
	Enabled  bool
	Sampling quality.Config
	Worker   quality.WorkerConfig
	Judge    QualityJudge
	Feedback QualityFeedback
}

// QualityFeedback tunes how Step 9.3's two feedback loops present the scores.
// Neither loop acts on the signal automatically — these only shape what counts
// as "low" and how many examples the routing loop lists.
type QualityFeedback struct {
	LowScoreThreshold float64
	ExampleLimit      int
}

// QualityJudge names the model that grades sampled responses and how long one
// grading call may take. The model must be a real name from providers.yaml —
// a routing keyword ("auto", a tier) is rejected at startup.
type QualityJudge struct {
	Model   string
	Timeout time.Duration
}

// --- on-disk shape ----------------------------------------------------------

type qualityFile struct {
	Quality qualityEntry `yaml:"quality"`
}

type qualityEntry struct {
	// Enabled defaults to true when absent, matching cache.yaml and
	// router.yaml's treatment of the same key.
	Enabled  *bool `yaml:"enabled"`
	Sampling struct {
		RoutedRate        float64 `yaml:"routed_rate"`
		NearThresholdBand float32 `yaml:"near_threshold_band"`
	} `yaml:"sampling"`
	Worker struct {
		QueueSize    int    `yaml:"queue_size"`
		Concurrency  int    `yaml:"concurrency"`
		ScoreTimeout string `yaml:"score_timeout"`
	} `yaml:"worker"`
	Judge struct {
		Model   string `yaml:"model"`
		Timeout string `yaml:"timeout"`
	} `yaml:"judge"`
	Feedback struct {
		LowScoreThreshold float64 `yaml:"low_score_threshold"`
		ExampleLimit      int     `yaml:"example_limit"`
	} `yaml:"feedback"`
}

// LoadQuality reads, validates, and resolves configs/quality.yaml.
//
// A missing file is not an error: it means quality verification is off, which
// is the pre-Phase-9 behaviour and must stay a supported configuration.
func LoadQuality(path string) (Quality, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Quality{Enabled: false}, nil
		}
		return Quality{}, fmt.Errorf("reading quality config: %w", err)
	}

	var file qualityFile
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()); err != nil {
		return Quality{}, fmt.Errorf("parsing %s:\n%s", path, yaml.FormatError(err, false, true))
	}

	e := file.Quality
	out := Quality{Enabled: enabledOrDefault(e.Enabled)}
	if !out.Enabled {
		return out, nil
	}

	if e.Sampling.RoutedRate < 0 || e.Sampling.RoutedRate > 1 {
		return Quality{}, fmt.Errorf("%s: sampling.routed_rate must be between 0 and 1, got %v", path, e.Sampling.RoutedRate)
	}
	if e.Sampling.NearThresholdBand < 0 || e.Sampling.NearThresholdBand > 1 {
		return Quality{}, fmt.Errorf("%s: sampling.near_threshold_band must be between 0 and 1, got %v", path, e.Sampling.NearThresholdBand)
	}

	if e.Judge.Model == "" {
		return Quality{}, fmt.Errorf("%s: judge.model is required when quality verification is enabled", path)
	}

	scoreTimeout, err := parseQualityDuration(path, "worker.score_timeout", e.Worker.ScoreTimeout, 20*time.Second)
	if err != nil {
		return Quality{}, err
	}
	judgeTimeout, err := parseQualityDuration(path, "judge.timeout", e.Judge.Timeout, 20*time.Second)
	if err != nil {
		return Quality{}, err
	}
	if e.Worker.QueueSize < 0 || e.Worker.Concurrency < 0 {
		return Quality{}, fmt.Errorf("%s: worker.queue_size and worker.concurrency must not be negative", path)
	}

	out.Sampling = quality.Config{
		RoutedRate:        e.Sampling.RoutedRate,
		NearThresholdBand: e.Sampling.NearThresholdBand,
	}
	out.Worker = quality.WorkerConfig{
		QueueSize:    e.Worker.QueueSize,
		Concurrency:  e.Worker.Concurrency,
		ScoreTimeout: scoreTimeout,
	}
	out.Judge = QualityJudge{Model: e.Judge.Model, Timeout: judgeTimeout}

	lowScore := e.Feedback.LowScoreThreshold
	if lowScore == 0 {
		lowScore = 3
	}
	if lowScore < 1 || lowScore > 5 {
		return Quality{}, fmt.Errorf("%s: feedback.low_score_threshold must be between 1 and 5, got %v", path, lowScore)
	}
	exampleLimit := e.Feedback.ExampleLimit
	if exampleLimit == 0 {
		exampleLimit = 20
	}
	if exampleLimit < 0 {
		return Quality{}, fmt.Errorf("%s: feedback.example_limit must not be negative", path)
	}
	out.Feedback = QualityFeedback{LowScoreThreshold: lowScore, ExampleLimit: exampleLimit}
	return out, nil
}

func parseQualityDuration(path, field, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %s %q is not a duration: %w", path, field, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: %s must be positive", path, field)
	}
	return d, nil
}
