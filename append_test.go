package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func useTestRetryHooks(
	t *testing.T,
	queryFn func(string, string) (string, bool, time.Duration, error),
	sleeper func(time.Duration),
) {
	t.Helper()
	oldQueryOnceFn := queryOnceFn
	oldSleepFn := sleepFn
	queryOnceFn = queryFn
	sleepFn = sleeper
	t.Cleanup(func() {
		queryOnceFn = oldQueryOnceFn
		sleepFn = oldSleepFn
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

func TestQueryPullsOnceRequestTimeoutStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte("timeout"))
	}))
	t.Cleanup(srv.Close)
	useTestHTTPClient(t, srv.Client())

	_, retry, _, err := queryPullsOnce("stunnerd", srv.URL)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !retry {
		t.Fatalf("expected retry=true for 408")
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

	if got := parseRetryAfter("999"); got != maxRetryAfter {
		t.Fatalf("expected capped delay %s, got %s", maxRetryAfter, got)
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

func TestIsRetryableRequestError(t *testing.T) {
	if !isRetryableRequestError(context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded to be retryable")
	}
	if isRetryableRequestError(context.Canceled) {
		t.Fatalf("expected context canceled to be non-retryable")
	}
	if !isRetryableRequestError(io.ErrUnexpectedEOF) {
		t.Fatalf("expected unexpected EOF to be retryable")
	}
	if isRetryableRequestError(errors.New("permanent error")) {
		t.Fatalf("expected generic error to be non-retryable")
	}
}

func TestHasDateInRecentRows(t *testing.T) {
	content := "date,repo\n" +
		"2026-04-20,1\n" +
		"2026-04-21,2\n" +
		"2026-04-22,3\n" +
		"2026-04-23,4\n"

	path := filepath.Join(t.TempDir(), "pull-stats.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("unable to write test csv: %v", err)
	}

	found, err := hasDateInRecentRows(path, "2026-04-23", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatalf("expected to find date in recent rows")
	}

	found, err = hasDateInRecentRows(path, "2026-04-20", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected old date to be outside recent row window")
	}
}

func TestGetDateWriteMode(t *testing.T) {
	t.Setenv(dateWriteModeEnv, "")
	if mode := getDateWriteMode(); mode != dateWriteModeSkip {
		t.Fatalf("expected default mode %q, got %q", dateWriteModeSkip, mode)
	}

	t.Setenv(dateWriteModeEnv, string(dateWriteModeAppend))
	if mode := getDateWriteMode(); mode != dateWriteModeAppend {
		t.Fatalf("expected mode %q, got %q", dateWriteModeAppend, mode)
	}

	t.Setenv(dateWriteModeEnv, "invalid")
	if mode := getDateWriteMode(); mode != dateWriteModeSkip {
		t.Fatalf("expected fallback mode %q, got %q", dateWriteModeSkip, mode)
	}
}

func TestOverwriteDateInRecentRows(t *testing.T) {
	content := "date,repo\n" +
		"2026-04-20,1\n" +
		"2026-04-21,2\n" +
		"2026-04-22,3\n" +
		"2026-04-23,4\n"

	path := filepath.Join(t.TempDir(), "pull-stats.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("unable to write test csv: %v", err)
	}

	overwritten, err := overwriteDateInRecentRows(path, []string{"2026-04-23", "99"}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !overwritten {
		t.Fatalf("expected overwrite to happen")
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unable to read updated file: %v", err)
	}
	if !strings.Contains(string(updated), "2026-04-23,99") {
		t.Fatalf("expected updated row in file, got: %s", string(updated))
	}
}

func TestOverwriteDateInRecentRowsOutsideWindow(t *testing.T) {
	content := "date,repo\n" +
		"2026-04-20,1\n" +
		"2026-04-21,2\n" +
		"2026-04-22,3\n" +
		"2026-04-23,4\n"

	path := filepath.Join(t.TempDir(), "pull-stats.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("unable to write test csv: %v", err)
	}

	overwritten, err := overwriteDateInRecentRows(path, []string{"2026-04-20", "99"}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overwritten {
		t.Fatalf("expected no overwrite outside recent window")
	}
}

func TestAppendRowWidthGuard(t *testing.T) {
	content := "date,repoA,repoB\n2026-04-23,1,2\n"
	path := filepath.Join(t.TempDir(), "pull-stats.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("unable to write test csv: %v", err)
	}

	err := appendRow(path, []string{"2026-04-24", "3"})
	if err == nil {
		t.Fatalf("expected width mismatch error")
	}
	if !strings.Contains(err.Error(), "row width mismatch") {
		t.Fatalf("expected width mismatch error, got: %v", err)
	}
}

func TestAppendRowSuccess(t *testing.T) {
	content := "date,repoA,repoB\n2026-04-23,1,2\n"
	path := filepath.Join(t.TempDir(), "pull-stats.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("unable to write test csv: %v", err)
	}

	err := appendRow(path, []string{"2026-04-24", "3", "4"})
	if err != nil {
		t.Fatalf("expected append success, got: %v", err)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unable to read updated file: %v", err)
	}
	if !strings.Contains(string(updated), "2026-04-24,3,4") {
		t.Fatalf("expected appended row in file, got: %s", string(updated))
	}
}

func TestQueryPullsRetriesThenSucceeds(t *testing.T) {
	attempts := 0
	sleeps := make([]time.Duration, 0, 2)
	useTestRetryHooks(
		t,
		func(_ string, _ string) (string, bool, time.Duration, error) {
			attempts++
			if attempts < 3 {
				return "", true, maxRetryAfter + time.Second, errors.New("temporary failure")
			}
			return "42", false, 0, nil
		},
		func(delay time.Duration) {
			sleeps = append(sleeps, delay)
		},
	)

	pulls, err := queryPulls("stunnerd")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if pulls != "42" {
		t.Fatalf("expected pulls=42, got %s", pulls)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if len(sleeps) != 2 {
		t.Fatalf("expected 2 sleeps, got %d", len(sleeps))
	}
	for _, delay := range sleeps {
		if delay != maxRetryAfter {
			t.Fatalf("expected capped sleep %s, got %s", maxRetryAfter, delay)
		}
	}
}

func TestQueryPullsStopsOnNonRetryableError(t *testing.T) {
	attempts := 0
	sleepCalls := 0
	useTestRetryHooks(
		t,
		func(_ string, _ string) (string, bool, time.Duration, error) {
			attempts++
			return "", false, 0, errors.New("bad request")
		},
		func(time.Duration) {
			sleepCalls++
		},
	)

	_, err := queryPulls("stunnerd")
	if err == nil {
		t.Fatalf("expected an error")
	}
	if attempts != 1 {
		t.Fatalf("expected a single attempt, got %d", attempts)
	}
	if sleepCalls != 0 {
		t.Fatalf("expected no retries/sleeps, got %d", sleepCalls)
	}
}

func TestQueryPullsExhaustsRetries(t *testing.T) {
	attempts := 0
	sleepCalls := 0
	useTestRetryHooks(
		t,
		func(_ string, _ string) (string, bool, time.Duration, error) {
			attempts++
			return "", true, 0, errors.New("still failing")
		},
		func(time.Duration) {
			sleepCalls++
		},
	)

	_, err := queryPulls("stunnerd")
	if err == nil {
		t.Fatalf("expected an error")
	}
	if attempts != maxQueryAttempts {
		t.Fatalf("expected %d attempts, got %d", maxQueryAttempts, attempts)
	}
	if sleepCalls != maxQueryAttempts-1 {
		t.Fatalf("expected %d sleeps, got %d", maxQueryAttempts-1, sleepCalls)
	}
	if !strings.Contains(err.Error(), "unable to query stunnerd after") {
		t.Fatalf("expected wrapped retry error, got %v", err)
	}
}
