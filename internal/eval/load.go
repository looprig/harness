package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// goldenCase is the on-disk JSON shape of a TestCase.
type goldenCase struct {
	Name           string   `json:"name"`
	Input          string   `json:"input"`
	ExpectedOutput string   `json:"expectedOutput,omitempty"`
	Context        []string `json:"context,omitempty"`
}

// LoadCases reads every *.json file in dir (non-recursively) as a TestCase.
// os.ReadDir returns entries sorted by filename, so ordering is deterministic.
// A missing dir, an unreadable file, or malformed JSON is a LoadError naming the
// offending path.
func LoadCases(dir string) ([]TestCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, &LoadError{Path: dir, Cause: err}
	}
	var cases []TestCase
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &LoadError{Path: path, Cause: err}
		}
		var gc goldenCase
		if err := json.Unmarshal(data, &gc); err != nil {
			return nil, &LoadError{Path: path, Cause: err}
		}
		cases = append(cases, TestCase{
			Name:           gc.Name,
			Input:          gc.Input,
			ExpectedOutput: gc.ExpectedOutput,
			Context:        gc.Context,
		})
	}
	return cases, nil
}
