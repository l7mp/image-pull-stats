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
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

type dockerHubRepository struct {
	PullCount *int `json:"pull_count"`
}

func readRepoNames(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck
	defer file.Close()

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

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		if closeErr := res.Body.Close(); closeErr != nil {
			return "", closeErr
		}

		return "", fmt.Errorf("unexpected status %d for %s: %s", res.StatusCode, repo, string(body))
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if err := res.Body.Close(); err != nil {
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

func main() {
	fileName := "pull-stats.csv"

	repos, err := readRepoNames(fileName)
	if err != nil {
		log.Fatalf("Unable to read %s: %s", fileName, err.Error())
	}

	row := []string{time.Now().Format("2006-01-02")}
	for _, r := range repos {
		pulls, err := queryPulls(r)
		if err != nil {
			log.Fatalf("Unable to query %s: %s", r, err.Error())
		}
		row = append(row, pulls)
	}

	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Unable to open %s: %s", fileName, err.Error())
	}
	//nolint:errcheck
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write(row); err != nil {
		log.Fatalf("Unable to write data: %s", err.Error())
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Fatalf("Unable to write data: %s", err.Error())
	}
}
