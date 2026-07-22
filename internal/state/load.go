package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// LoadPrev reads a previous registry.json from disk. A missing file, an
// empty file, or a `{}` file are all treated as "no prior state" so the
// crawler's very first run has a well-defined starting point. Everything
// else (malformed JSON, wrong schema version) surfaces as an error and the
// crawler must fail-close per 03_crawler.md.
func LoadPrev(path string) (*manifest.RegistryDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyDoc(), nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return emptyDoc(), nil
	}

	// A literal "{}" file is how the GHA workflow seeds a fresh state
	// (03_crawler.md). Treat it as "no prior state" before the
	// schema-version guard rejects it as SchemaVersion == 0.
	if bytes.Equal(bytes.TrimSpace(data), []byte("{}")) {
		return emptyDoc(), nil
	}
	var doc manifest.RegistryDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.SchemaVersion != manifest.CurrentRegistrySchemaVersion {
		return nil, &SchemaMismatchError{Got: doc.SchemaVersion, Want: manifest.CurrentRegistrySchemaVersion}
	}
	return &doc, nil
}

// SchemaMismatchError signals that the previous registry was written by a
// crawler build that speaks a different schema_version. The current process
// should refuse to continue rather than silently corrupt state.
type SchemaMismatchError struct {
	Got  int
	Want int
}

func (e *SchemaMismatchError) Error() string {
	return fmt.Sprintf("prev registry schema_version %d is not understood by this crawler (want %d)", e.Got, e.Want)
}

func emptyDoc() *manifest.RegistryDocument {
	return &manifest.RegistryDocument{
		SchemaVersion: manifest.CurrentRegistrySchemaVersion,
		Plugins:       []manifest.RegistryEntry{},
	}
}
