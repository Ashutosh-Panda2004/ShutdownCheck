package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shutdowncheck/shutdowncheck/internal/config"
)

// Body limits bound both an individual payload and their retained aggregate.
const (
	MaxRequestBodyBytes      = 10 << 20
	MaxTotalRequestBodyBytes = 32 << 20
)

func readRequestBody(path string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path)) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, MaxRequestBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRequestBodyBytes {
		return nil, fmt.Errorf("request body exceeds the %d-byte limit", MaxRequestBodyBytes)
	}
	return data, nil
}

func materializeRequestBodies(resolved *config.Resolved, baseDir string) error {
	total := 0
	for i := range resolved.Probe.Requests {
		request := &resolved.Probe.Requests[i]
		if request.BodyFile == "" {
			total += len(request.Body)
			if total > MaxTotalRequestBodyBytes {
				return fmt.Errorf("request bodies exceed the %d-byte aggregate limit", MaxTotalRequestBodyBytes)
			}
			continue
		}

		path := request.BodyFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		data, err := readRequestBody(path)
		if err != nil {
			return fmt.Errorf("read body_file for request %q: %w", request.Name, err)
		}
		total += len(data)
		if total > MaxTotalRequestBodyBytes {
			return fmt.Errorf("request bodies exceed the %d-byte aggregate limit", MaxTotalRequestBodyBytes)
		}
		request.Body = string(data)
		request.BodyFile = ""
	}
	return nil
}
