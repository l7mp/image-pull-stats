package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func useTestHTTPClient(t *testing.T, client *http.Client) {
	t.Helper()
	oldClient := httpClient
	httpClient = client
	t.Cleanup(func() {
		httpClient = oldClient
	})
}

func TestQueryPullsOnceSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pull_count":12345}`))
	}))
	t.Cleanup(srv.Close)
	useTestHTTPClient(t, srv.Client())

	pulls, retry, retryAfter, err := queryPullsOnce("stunnerd", srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if pulls != "12345" {
		t.Fatalf("expected pulls=12345, got %s", pulls)
	}
	if retry {
		t.Fatalf("expected retry=false")
	}
	if retryAfter != 0 {
		t.Fatalf("expected retryAfter=0, got %s", retryAfter)
	}
}

func TestQueryPullsOnceRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	t.Cleanup(srv.Close)
	useTestHTTPClient(t, srv.Client())

	_, retry, retryAfter, err := queryPullsOnce("stunnerd", srv.URL)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !retry {
		t.Fatalf("expected retry=true")
	}
	if retryAfter != 2*time.Second {
		t.Fatalf("expected retryAfter=2s, got %s", retryAfter)
	}
}

func TestQueryPullsOnceNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	t.Cleanup(srv.Close)
	useTestHTTPClient(t, srv.Client())

	_, retry, _, err := queryPullsOnce("stunnerd", srv.URL)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if retry {
		t.Fatalf("expected retry=false")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("3"); got != 3*time.Second {
		t.Fatalf("expected 3s, got %s", got)
	}

	httpDate := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(httpDate)
	if got <= 0 {
		t.Fatalf("expected positive duration, got %s", got)
	}

	if got := parseRetryAfter("invalid"); got != 0 {
		t.Fatalf("expected 0 for invalid Retry-After, got %s", got)
	}
}

func TestBackoffDelayRange(t *testing.T) {
	for attempt := 1; attempt <= 8; attempt++ {
		maxDelay := baseRetryDelay << (attempt - 1)
		if maxDelay > maxRetryDelay {
			maxDelay = maxRetryDelay
		}
		minDelay := maxDelay / 2

		for i := 0; i < 20; i++ {
			delay := backoffDelay(attempt)
			if delay < minDelay || delay > maxDelay {
				t.Fatalf("attempt %d: delay %s out of range [%s, %s]", attempt, delay, minDelay, maxDelay)
			}
		}
	}
}
