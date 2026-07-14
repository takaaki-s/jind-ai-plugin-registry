// Package output serialises the merged registry document to disk. It exists
// so that the marshalling contract (indent style, key ordering, atomic
// write) has a single home and can be swapped without touching the crawler.
package output

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// Write serialises doc as pretty-printed JSON and writes it to path. The
// write is atomic (temp file + rename in the same directory) so a partial
// registry never gets picked up by GitHub Pages if the process is killed
// mid-write.
func Write(path string, doc *manifest.RegistryDocument) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
