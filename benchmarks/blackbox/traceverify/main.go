// Command traceverify reconciles black-box request evidence with a canonical
// SparkRoute JSONL trace export. It fails if any gateway-handled request is
// missing or duplicated; optional strict mode also rejects unrelated traces.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type requestEvidence struct {
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
}

type traceEvidence struct {
	RequestID string `json:"request_id"`
}

type result struct {
	SchemaVersion      int      `json:"schema_version"`
	ExpectedRequests   int      `json:"expected_requests"`
	EvidenceWithoutID  int      `json:"request_evidence_without_id"`
	DuplicateEvidence  int      `json:"duplicate_request_evidence_ids"`
	ObservedTraces     int      `json:"observed_traces"`
	MatchedRequests    int      `json:"matched_requests"`
	MissingRequestIDs  int      `json:"missing_request_ids"`
	DuplicateRequestID int      `json:"duplicate_request_ids"`
	UnexpectedTraces   int      `json:"unexpected_traces"`
	Missing            []string `json:"missing,omitempty"`
	Duplicates         []string `json:"duplicates,omitempty"`
	Unexpected         []string `json:"unexpected,omitempty"`
	EvidenceDuplicates []string `json:"evidence_duplicates,omitempty"`
	Complete           bool     `json:"complete"`
}

func main() {
	var requestsPaths pathFlags
	var tracesPath, outputPath string
	var allowExtra bool
	flag.Var(&requestsPaths, "requests", "loadgen request JSONL; repeatable")
	flag.StringVar(&tracesPath, "traces", "", "canonical trace export JSONL")
	flag.StringVar(&outputPath, "output", "-", "verification JSON path or - for stdout")
	flag.BoolVar(&allowExtra, "allow-extra", false, "allow exported traces not present in request evidence")
	flag.Parse()
	verification, err := verify(requestsPaths, tracesPath, allowExtra)
	if err != nil {
		fmt.Fprintln(os.Stderr, "traceverify:", err)
		os.Exit(1)
	}
	if err := emit(outputPath, verification); err != nil {
		fmt.Fprintln(os.Stderr, "traceverify:", err)
		os.Exit(1)
	}
	if !verification.Complete {
		os.Exit(1)
	}
}

func verify(requestsPaths []string, tracesPath string, allowExtra bool) (result, error) {
	if len(requestsPaths) == 0 || strings.TrimSpace(tracesPath) == "" {
		return result{}, fmt.Errorf("-requests and -traces are required")
	}
	expected := make(map[string]int)
	evidenceWithoutID := 0
	for _, requestsPath := range requestsPaths {
		ids, withoutID, err := readRequestIDs(requestsPath)
		if err != nil {
			return result{}, fmt.Errorf("read requests %q: %w", requestsPath, err)
		}
		for id, count := range ids {
			expected[id] += count
		}
		evidenceWithoutID += withoutID
	}
	observed, observedRows, err := readIDs(tracesPath, func(raw []byte) (string, error) {
		var record traceEvidence
		if err := json.Unmarshal(raw, &record); err != nil {
			return "", err
		}
		return record.RequestID, nil
	})
	if err != nil {
		return result{}, fmt.Errorf("read traces: %w", err)
	}
	missing := make([]string, 0)
	evidenceDuplicates := make([]string, 0)
	duplicates := make([]string, 0)
	matched := 0
	for id, count := range expected {
		if count != 1 {
			evidenceDuplicates = append(evidenceDuplicates, id)
		}
		switch observed[id] {
		case 0:
			missing = append(missing, id)
		case 1:
			matched++
		default:
			duplicates = append(duplicates, id)
		}
	}
	unexpected := make([]string, 0)
	for id := range observed {
		if expected[id] == 0 {
			unexpected = append(unexpected, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(evidenceDuplicates)
	sort.Strings(duplicates)
	sort.Strings(unexpected)
	verification := result{
		SchemaVersion:      1,
		ExpectedRequests:   len(expected),
		EvidenceWithoutID:  evidenceWithoutID,
		DuplicateEvidence:  len(evidenceDuplicates),
		ObservedTraces:     observedRows,
		MatchedRequests:    matched,
		MissingRequestIDs:  len(missing),
		DuplicateRequestID: len(duplicates),
		UnexpectedTraces:   len(unexpected),
		Missing:            bounded(missing, 100),
		Duplicates:         bounded(duplicates, 100),
		Unexpected:         bounded(unexpected, 100),
		EvidenceDuplicates: bounded(evidenceDuplicates, 100),
	}
	verification.Complete = len(expected) > 0 && evidenceWithoutID == 0 &&
		len(evidenceDuplicates) == 0 && len(missing) == 0 &&
		len(duplicates) == 0 && (allowExtra || len(unexpected) == 0)
	return verification, nil
}

type pathFlags []string

func (p *pathFlags) String() string {
	return strings.Join(*p, ",")
}

func (p *pathFlags) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("path must not be empty")
	}
	*p = append(*p, value)
	return nil
}

func readRequestIDs(path string) (map[string]int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	result := make(map[string]int)
	withoutID := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	line := 0
	for scanner.Scan() {
		line++
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var record requestEvidence
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, 0, fmt.Errorf("line %d: %w", line, err)
		}
		id := strings.TrimSpace(record.RequestID)
		if id != "" {
			result[id]++
		} else if record.Status != 0 {
			withoutID++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return result, withoutID, nil
}

func readIDs(
	path string,
	decode func([]byte) (string, error),
) (map[string]int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	result := make(map[string]int)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	line := 0
	rows := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		id, err := decode(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("line %d: %w", line, err)
		}
		rows++
		id = strings.TrimSpace(id)
		if id != "" {
			result[id]++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return result, rows, nil
}

func bounded(values []string, maximum int) []string {
	if len(values) == 0 {
		return nil
	}
	if len(values) > maximum {
		values = values[:maximum]
	}
	return values
}

func emit(path string, value any) error {
	var writer io.Writer = os.Stdout
	var file *os.File
	if path != "-" {
		var err error
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		writer = file
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
