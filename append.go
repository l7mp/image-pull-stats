package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

const (
	maxQueryAttempts  = 4
	baseRetryDelay    = 500 * time.Millisecond
	maxRetryDelay     = 5 * time.Second
	maxRetryAfter     = 30 * time.Second
	recentRowsToCheck = 16
)

var (
	jitterRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	jitterMu   sync.Mutex
)

type dockerHubRepository struct {
	PullCount *int `json:"pull_count"`
}

type pullJob struct {
	index int
	repo  string
}

type pullResult struct {
	index int
	pulls string
	err   error
}

func readRepoNames(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			log.Printf("Unable to close %s: %s", path, closeErr.Error())
		}
	}()

	reader := csv.NewReader(file)
	column, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if len(column) < 2 {
		return nil, fmt.Errorf("missing repository columns in %s", path)
	}

	return column[1:], nil
}

func hasDateInRecentRows(path, date string, rows int) (bool, error) {
	if rows <= 0 {
		return false, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			log.Printf("Unable to close %s: %s", path, closeErr.Error())
		}
	}()

	reader := csv.NewReader(file)
	headers, err := reader.Read()
	if err != nil {
		return false, err
	}
	if len(headers) == 0 || headers[0] != "date" {
		return false, fmt.Errorf("invalid csv header in %s", path)
	}

	recent := make([]string, 0, rows)
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return false, readErr
		}
		if len(record) == 0 {
			continue
		}

		recent = append(recent, record[0])
		if len(recent) > rows {
			recent = recent[1:]
		}
	}

	for _, rowDate := range recent {
		if rowDate == date {
			return true, nil
		}
	}

	return false, nil
}

func queryPulls(repo string) (string, error) {
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/l7mp/%s", repo)
	var lastErr error

	for attempt := 1; attempt <= maxQueryAttempts; attempt++ {
		pulls, retry, retryAfter, err := queryPullsOnce(repo, url)
		if err == nil {
			return pulls, nil
		}

		lastErr = err
		if !retry || attempt == maxQueryAttempts {
			break
		}

		delay := backoffDelay(attempt)
		if retryAfter > delay {
			delay = retryAfter
		}

		if delay > maxRetryAfter {
			delay = maxRetryAfter
		}

		log.Printf("Retrying %s in %s (attempt %d/%d): %s", repo, delay, attempt+1, maxQueryAttempts, err.Error())
		time.Sleep(delay)
	}

	return "", fmt.Errorf("unable to query %s after %d attempts: %w", repo, maxQueryAttempts, lastErr)
}

func queryPullsOnce(repo, url string) (string, bool, time.Duration, error) {
	res, err := httpClient.Get(url)
	if err != nil {
		return "", isRetryableRequestError(err), 0, err
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			log.Printf("Unable to close response body for %s: %s", repo, closeErr.Error())
		}
	}()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		retryAfter := parseRetryAfter(res.Header.Get("Retry-After"))
		err := fmt.Errorf("unexpected status %d for %s: %s", res.StatusCode, repo, string(body))

		return "", isRetryableStatusCode(res.StatusCode), retryAfter, err
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", true, 0, err
	}

	var repository dockerHubRepository
	if err := json.Unmarshal(body, &repository); err != nil {
		return "", false, 0, fmt.Errorf("unable to parse response for %s: %w", repo, err)
	}
	if repository.PullCount == nil {
		return "", false, 0, fmt.Errorf("pull_count not found for %s", repo)
	}

	return strconv.Itoa(*repository.PullCount), false, 0, nil
}

func backoffDelay(attempt int) time.Duration {
	delay := baseRetryDelay << (attempt - 1)
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}

	minDelay := delay / 2
	if minDelay == 0 {
		return delay
	}

	jitterMu.Lock()
	jitter := time.Duration(jitterRand.Int63n(int64(delay-minDelay) + 1))
	jitterMu.Unlock()

	return minDelay + jitter
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}

	seconds, err := strconv.Atoi(value)
	if err == nil {
		if seconds > 0 {
			delay := time.Duration(seconds) * time.Second
			if delay > maxRetryAfter {
				return maxRetryAfter
			}

			return delay
		}

		return 0
	}

	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}

	delay := time.Until(retryAt)
	if delay < 0 {
		return 0
	}
	if delay > maxRetryAfter {
		return maxRetryAfter
	}

	return delay
}

func isRetryableStatusCode(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

func isRetryableRequestError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func queryAllPulls(repos []string) ([]string, error) {
	workers := 8
	if len(repos) < workers {
		workers = len(repos)
	}
	if workers == 0 {
		return nil, fmt.Errorf("no repositories configured")
	}

	jobs := make(chan pullJob, len(repos))
	results := make(chan pullResult, len(repos))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				pulls, err := queryPulls(job.repo)
				if err != nil {
					results <- pullResult{index: job.index, err: fmt.Errorf("%s: %w", job.repo, err)}
					continue
				}

				results <- pullResult{index: job.index, pulls: pulls}
			}
		}()
	}

	for i, repo := range repos {
		jobs <- pullJob{index: i, repo: repo}
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	pulls := make([]string, len(repos))
	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		pulls[result.index] = result.pulls
	}

	if firstErr != nil {
		return nil, firstErr
	}

	return pulls, nil
}

func main() {
	fileName := "pull-stats.csv"
	today := time.Now().UTC().Format("2006-01-02")

	exists, err := hasDateInRecentRows(fileName, today, recentRowsToCheck)
	if err != nil {
		log.Fatalf("Unable to inspect %s: %s", fileName, err.Error())
	}
	if exists {
		log.Printf("Data point already exists for %s, skipping update", today)
		return
	}

	repos, err := readRepoNames(fileName)
	if err != nil {
		log.Fatalf("Unable to read %s: %s", fileName, err.Error())
	}

	pulls, err := queryAllPulls(repos)
	if err != nil {
		log.Fatalf("Unable to query repositories: %s", err.Error())
	}

	row := []string{today}
	row = append(row, pulls...)

	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Unable to open %s: %s", fileName, err.Error())
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			log.Printf("Unable to close %s: %s", fileName, closeErr.Error())
		}
	}()

	w := csv.NewWriter(f)
	if err := w.Write(row); err != nil {
		log.Fatalf("Unable to write data: %s", err.Error())
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Fatalf("Unable to write data: %s", err.Error())
	}
}
