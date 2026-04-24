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
	"path/filepath"
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
	dateWriteModeEnv  = "DATE_WRITE_MODE"
)

var (
	jitterRand  = rand.New(rand.NewSource(time.Now().UnixNano()))
	jitterMu    sync.Mutex
	sleepFn     = time.Sleep
	queryOnceFn = queryPullsOnce
)

type dateWriteMode string

const (
	dateWriteModeSkip      dateWriteMode = "skip"
	dateWriteModeAppend    dateWriteMode = "append"
	dateWriteModeOverwrite dateWriteMode = "overwrite"
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

func getDateWriteMode() dateWriteMode {
	mode := dateWriteMode(os.Getenv(dateWriteModeEnv))
	if mode == "" {
		return dateWriteModeSkip
	}

	switch mode {
	case dateWriteModeSkip, dateWriteModeAppend, dateWriteModeOverwrite:
		return mode
	default:
		log.Printf("Unknown %s=%q, falling back to %q", dateWriteModeEnv, mode, dateWriteModeSkip)
		return dateWriteModeSkip
	}
}

func appendRow(path string, row []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			log.Printf("Unable to close %s: %s", path, closeErr.Error())
		}
	}()

	w := csv.NewWriter(f)
	if err := w.Write(row); err != nil {
		return err
	}
	w.Flush()

	return w.Error()
}

func overwriteDateInRecentRows(path string, row []string, rows int) (bool, error) {
	if len(row) == 0 {
		return false, fmt.Errorf("missing row date")
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

	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return false, fmt.Errorf("empty csv in %s", path)
	}
	if len(records[0]) == 0 || records[0][0] != "date" {
		return false, fmt.Errorf("invalid csv header in %s", path)
	}
	if len(row) != len(records[0]) {
		return false, fmt.Errorf("row width mismatch: got %d values, want %d", len(row), len(records[0]))
	}

	start := 1
	if rows > 0 && len(records)-rows > start {
		start = len(records) - rows
	}

	replaceIndex := -1
	for i := len(records) - 1; i >= start; i-- {
		if len(records[i]) > 0 && records[i][0] == row[0] {
			replaceIndex = i
			break
		}
	}

	if replaceIndex == -1 {
		return false, nil
	}

	records[replaceIndex] = row

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer func() {
		if removeErr := os.Remove(tmpPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Printf("Unable to remove temp file %s: %s", tmpPath, removeErr.Error())
		}
	}()

	w := csv.NewWriter(tmp)
	if err := w.WriteAll(records); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := w.Error(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return false, err
	}

	return true, nil
}

func queryPulls(repo string) (string, error) {
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/l7mp/%s", repo)
	var lastErr error

	for attempt := 1; attempt <= maxQueryAttempts; attempt++ {
		pulls, retry, retryAfter, err := queryOnceFn(repo, url)
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
		sleepFn(delay)
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
	mode := getDateWriteMode()

	if mode == dateWriteModeSkip {
		exists, err := hasDateInRecentRows(fileName, today, recentRowsToCheck)
		if err != nil {
			log.Fatalf("Unable to inspect %s: %s", fileName, err.Error())
		}
		if exists {
			log.Printf("Data point already exists for %s, skipping update", today)
			return
		}
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

	switch mode {
	case dateWriteModeOverwrite:
		overwritten, err := overwriteDateInRecentRows(fileName, row, recentRowsToCheck)
		if err != nil {
			log.Fatalf("Unable to overwrite data in %s: %s", fileName, err.Error())
		}
		if overwritten {
			log.Printf("Overwrote existing data point for %s", today)
			return
		}
		if err := appendRow(fileName, row); err != nil {
			log.Fatalf("Unable to append data in %s: %s", fileName, err.Error())
		}
	default:
		if err := appendRow(fileName, row); err != nil {
			log.Fatalf("Unable to append data in %s: %s", fileName, err.Error())
		}
	}
}
