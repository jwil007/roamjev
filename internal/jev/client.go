// Package jev is a minimal client for TypeSafe's System One API, which serves
// the Jev decision model. TypeSafe publishes Python and JavaScript SDKs only,
// so this wraps the REST endpoint directly.
//
// Reference: https://docs.typesafe.ai/api.md
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"
	// Pin the versioned ID so behavior doesn't shift under an experiment.
	// See https://docs.typesafe.ai/models.md
	DefaultModel = "jev-1.13.0"
	// $0.042 per million input tokens; output tokens are free.
	USDPerInputToken = 0.042 / 1_000_000
)

// Question is one typed question. Criteria is:
//   - choice: map[string]string of option -> description (max 255 options)
//   - score:  []string of ordered levels, low to high (2-10 levels)
//   - noul:   optional map with "true"/"false" descriptions
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func Choice(instructions string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

func Score(instructions string, levels []string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

func Noul(instructions string) Question {
	return Question{Type: "noul", Instructions: instructions}
}

type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer holds the union of the three answer shapes.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// APIError is a non-200 response. Status codes per the API reference:
// 401 bad key, 422 validation, 429 rate limited, 529 overloaded.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("jev: HTTP %d: %s", e.Status, e.Body)
}

func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == 529 ||
		e.Status >= 500
}

type Client struct {
	Endpoint string
	Model    string
	key      string
	http     *http.Client
}

func New(key string) *Client {
	return &Client{
		Endpoint: DefaultEndpoint,
		Model:    DefaultModel,
		key:      key,
		http:     &http.Client{},
	}
}

// LoadKey reads the API key from $TYPESAFE_API_KEY, falling back to path.
func LoadKey(path string) (string, error) {
	if k := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")); k != "" {
		return k, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read API key: %w", err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("API key file %s is empty", path)
	}
	return k, nil
}

// Result is a completed call with timing, for the decision journal.
type Result struct {
	Response
	Latency time.Duration
	CostUSD float64
}

// Evaluate sends one request. The caller bounds it with ctx; there is no
// retry here because a stale roaming decision is worse than none.
func (c *Client) Evaluate(ctx context.Context, state any,
	questions map[string]Question) (Result, error) {
	body, err := json.Marshal(Request{
		Model: c.Model, State: state, Questions: questions})
	if err != nil {
		return Result{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("jev request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := time.Since(start)
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, &APIError{Status: resp.StatusCode,
			Body: strings.TrimSpace(string(raw))}
	}
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		return Result{}, fmt.Errorf("decode response: %w", err)
	}
	if r.Answers == nil {
		return Result{}, errors.New("jev: response has no answers")
	}
	return Result{
		Response: r,
		Latency:  latency,
		CostUSD:  float64(r.Usage.InputTokens) * USDPerInputToken,
	}, nil
}
