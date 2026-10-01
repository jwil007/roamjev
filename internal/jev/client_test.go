package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != DefaultModel || req.Questions["a"].Type != "choice" {
			t.Errorf("unexpected request: %+v", req)
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"a":{"type":"choice","choice":"x","confidence":0.9,"probabilities":{"x":0.95,"y":0.05}}},"usage":{"input_tokens":1000,"output_tokens":10}}`))
	}))
	defer srv.Close()
	c := New("k")
	c.Endpoint = srv.URL
	res, err := c.Evaluate(context.Background(), map[string]any{"s": 1},
		map[string]Question{"a": Choice("q", map[string]string{"x": "", "y": ""})})
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Answers["a"]; a.Choice != "x" || a.Probabilities["x"] != 0.95 {
		t.Fatalf("answer = %+v", a)
	}
	if want := 1000 * USDPerInputToken; res.CostUSD != want {
		t.Fatalf("cost = %v, want %v", res.CostUSD, want)
	}

	c2 := New("bad")
	c2.Endpoint = srv.URL
	_, err = c2.Evaluate(context.Background(), "s", map[string]Question{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Fatalf("want 401 APIError, got %v", err)
	}
}

func TestEvaluateTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	c := New("k")
	c.Endpoint = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Evaluate(ctx, "s", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}
