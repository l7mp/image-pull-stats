package main

import (
	"encoding/json"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

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

func queryPulls(repo string) (string, error) {
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/l7mp/%s", repo)
	res, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			log.Printf("Unable to close response body for %s: %s", repo, closeErr.Error())
		}
	}()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("unexpected status %d for %s: %s", res.StatusCode, repo, string(body))
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	var repository dockerHubRepository
	if err := json.Unmarshal(body, &repository); err != nil {
		return "", fmt.Errorf("unable to parse response for %s: %w", repo, err)
	}
	if repository.PullCount == nil {
		return "", fmt.Errorf("pull_count not found for %s", repo)
	}

	return strconv.Itoa(*repository.PullCount), nil
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

	repos, err := readRepoNames(fileName)
	if err != nil {
		log.Fatalf("Unable to read %s: %s", fileName, err.Error())
	}

	pulls, err := queryAllPulls(repos)
	if err != nil {
		log.Fatalf("Unable to query repositories: %s", err.Error())
	}

	row := []string{time.Now().UTC().Format("2006-01-02")}
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
