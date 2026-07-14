package crawl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai-plugin-registry/internal/github"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// TestRun_EndToEnd walks the whole pipeline through an httptest.Server.
// The scenario deliberately mixes every kind of input the crawler must
// handle in a single run:
//
//   - alice/notifier      — valid, tagged release (should land)
//   - bob/logger          — valid, no releases (HEAD fallback should land)
//   - eve/broken          — manifest with a bad name (should be skipped)
//   - fork/of-notifier    — is:fork  (should be skipped by shouldSkip)
//   - archived/old-plugin — archived (should be skipped by shouldSkip)
//
// Prev registry additionally contains stale/foo, whose repo does not appear
// in this run — Merge should stamp orphaned_since=now on the carried entry.
func TestRun_EndToEnd(t *testing.T) {
	now := mustParse("2026-07-14T12:00:00Z")

	mux := http.NewServeMux()
	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				repoJSON("alice/notifier", "2025-01-01T00:00:00Z", "main", false, false),
				repoJSON("bob/logger", "2025-02-01T00:00:00Z", "main", false, false),
				repoJSON("eve/broken", "2025-03-01T00:00:00Z", "main", false, false),
				repoJSON("fork/of-notifier", "2025-04-01T00:00:00Z", "main", true, false),
				repoJSON("archived/old-plugin", "2025-05-01T00:00:00Z", "main", false, true),
			},
		})
	})

	// alice/notifier: one release v0.3.0 → SHA-a
	mux.HandleFunc("/repos/alice/notifier/releases", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v0.3.0", "draft": false, "prerelease": false},
		})
	})
	mux.HandleFunc("/repos/alice/notifier/commits/v0.3.0", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(commitJSON("sha-a", "2026-07-01T00:00:00Z"))
	})
	mux.HandleFunc("/raw/alice/notifier/sha-a/jind-ai-plugin.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validManifest("my-notifier")))
	})

	// bob/logger: no releases → fallback to default-branch HEAD.
	mux.HandleFunc("/repos/bob/logger/releases", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("/repos/bob/logger/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(commitJSON("sha-b", "2026-07-10T00:00:00Z"))
	})
	mux.HandleFunc("/raw/bob/logger/sha-b/jind-ai-plugin.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validManifest("event-logger")))
	})

	// eve/broken: manifest has an invalid name (uppercase not allowed).
	mux.HandleFunc("/repos/eve/broken/releases", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("/repos/eve/broken/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(commitJSON("sha-e", "2026-07-05T00:00:00Z"))
	})
	mux.HandleFunc("/raw/eve/broken/sha-e/jind-ai-plugin.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validManifest("BadName!!!"))) // fails NamePattern
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := github.NewHTTPClient(github.Config{
		Token:      "test-token",
		BaseURL:    srv.URL,
		RawBaseURL: srv.URL + "/raw",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	tmp := t.TempDir()
	prevPath := filepath.Join(tmp, "prev.json")
	outPath := filepath.Join(tmp, "registry.json")

	// Seed prev with a stale entry so we can verify orphan-transition.
	stalePrev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{{
			Name:           "stale-plugin",
			Repo:           "stale/foo",
			JinCompat:      ">=0.6.0",
			LatestVersion:  "0.1.0",
			Versions:       []manifest.RegistryVersion{{Version: "0.1.0", SHA: "old", Tag: "v0.1.0"}},
			FirstClaimedAt: mustParse("2025-12-01T00:00:00Z"),
		}},
	}
	prevBytes, _ := json.MarshalIndent(stalePrev, "", "  ")
	if err := os.WriteFile(prevPath, prevBytes, 0o644); err != nil {
		t.Fatalf("write prev: %v", err)
	}

	err = Run(context.Background(), Options{
		Client:   client,
		PrevPath: prevPath,
		OutPath:  outPath,
		Now:      func() time.Time { return now },
		Logger:   silentLogger(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := readDoc(t, outPath)

	if !got.GeneratedAt.Equal(now) {
		t.Errorf("GeneratedAt = %v, want %v", got.GeneratedAt, now)
	}
	if got.SchemaVersion != manifest.CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, manifest.CurrentSchemaVersion)
	}
	if len(got.Plugins) != 3 {
		t.Fatalf("want 3 plugins (alice + bob + stale-orphaned), got %d: %+v", len(got.Plugins), pluginNames(got.Plugins))
	}

	byName := map[string]manifest.RegistryEntry{}
	for _, p := range got.Plugins {
		byName[p.Name] = p
	}

	// Alice — tagged release path.
	if alice, ok := byName["my-notifier"]; !ok {
		t.Errorf("my-notifier entry missing; names=%v", pluginNames(got.Plugins))
	} else {
		if alice.Repo != "alice/notifier" || alice.LatestVersion != "0.3.0" {
			t.Errorf("alice entry wrong: %+v", alice)
		}
		if len(alice.Versions) != 1 || alice.Versions[0].SHA != "sha-a" {
			t.Errorf("alice versions wrong: %+v", alice.Versions)
		}
		// manifest_url must reflect the client's configured RawBaseURL so
		// mocks / staging mirrors do not leak the production host.
		wantURL := srv.URL + "/raw/alice/notifier/sha-a/jind-ai-plugin.yaml"
		if alice.Versions[0].ManifestURL != wantURL {
			t.Errorf("manifest_url = %s, want %s", alice.Versions[0].ManifestURL, wantURL)
		}
	}

	// Bob — HEAD fallback path.
	if bob, ok := byName["event-logger"]; !ok {
		t.Errorf("event-logger entry missing")
	} else {
		if bob.Repo != "bob/logger" || bob.LatestVersion != "0.0.0" || bob.Versions[0].Tag != "HEAD" {
			t.Errorf("bob HEAD fallback wrong: %+v", bob)
		}
	}

	// Stale — orphaned transition.
	if stale, ok := byName["stale-plugin"]; !ok {
		t.Errorf("stale-plugin entry should be preserved as orphaned")
	} else {
		if stale.OrphanedSince == nil || !stale.OrphanedSince.Equal(now) {
			t.Errorf("orphaned_since not stamped: %v", stale.OrphanedSince)
		}
	}

	// eve/broken must NOT appear.
	if _, ok := byName["BadName!!!"]; ok {
		t.Errorf("broken manifest should have been skipped")
	}
}

func TestRun_FailClose_OnPrevReadError(t *testing.T) {
	// Prev registry with an unknown schema_version → SchemaMismatchError.
	tmp := t.TempDir()
	prevPath := filepath.Join(tmp, "prev.json")
	bad := `{"schema_version": 99, "plugins": []}`
	if err := os.WriteFile(prevPath, []byte(bad), 0o644); err != nil {
		t.Fatalf("write prev: %v", err)
	}
	err := Run(context.Background(), Options{
		Client:   stubClient{},
		PrevPath: prevPath,
		OutPath:  filepath.Join(tmp, "out.json"),
		Logger:   silentLogger(),
	})
	if err == nil {
		t.Fatal("want fail-close error on unknown prev schema_version")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error should mention schema_version, got: %v", err)
	}
}

func TestRun_MissingRequiredFields(t *testing.T) {
	tmp := t.TempDir()
	err := Run(context.Background(), Options{})
	if err == nil {
		t.Error("want error when Client is nil")
	}
	err = Run(context.Background(), Options{Client: stubClient{}})
	if err == nil {
		t.Error("want error when PrevPath/OutPath empty")
	}
	// Sanity: with the minimal happy set + empty prev + zero repos, Run
	// should succeed and emit an empty registry.
	prevPath := filepath.Join(tmp, "prev.json")
	outPath := filepath.Join(tmp, "registry.json")
	if err := os.WriteFile(prevPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write prev: %v", err)
	}
	if err := Run(context.Background(), Options{
		Client:   stubClient{},
		PrevPath: prevPath,
		OutPath:  outPath,
		Now:      func() time.Time { return mustParse("2026-07-14T00:00:00Z") },
		Logger:   silentLogger(),
	}); err != nil {
		t.Fatalf("Run with empty inputs: %v", err)
	}
	doc := readDoc(t, outPath)
	if len(doc.Plugins) != 0 {
		t.Errorf("empty run should produce empty plugin list, got %d", len(doc.Plugins))
	}
}

// stubClient always returns empty results — useful for tests that only care
// about Run's non-search behavior (validation, output writing, prev-load).
type stubClient struct{}

func (stubClient) SearchByTopic(context.Context, string) ([]github.Repo, error) {
	return nil, nil
}
func (stubClient) GetManifest(context.Context, string, string, string) ([]byte, error) {
	return nil, github.ErrManifestMissing
}
func (stubClient) ListVersions(context.Context, string, string) ([]github.Version, error) {
	return nil, nil
}
func (stubClient) RawURL(repo, sha, path string) string {
	return "stub://" + repo + "/" + sha + "/" + path
}

// helpers ----

func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func repoJSON(fullName, createdAt, defaultBranch string, fork, archived bool) map[string]any {
	return map[string]any{
		"full_name":      fullName,
		"description":    "desc-" + fullName,
		"homepage":       "https://example.com/" + fullName,
		"fork":           fork,
		"archived":       archived,
		"private":        false,
		"created_at":     createdAt,
		"default_branch": defaultBranch,
	}
}

func commitJSON(sha, date string) map[string]any {
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"committer": map[string]any{"date": date},
		},
	}
}

func validManifest(name string) string {
	return fmt.Sprintf(`schema_version: 1
name: %s
version: 0.1.0
description: hello
license: MIT
homepage: https://example.com
jin: ">=0.7.0"
install:
  source:
    build:
      - go build -o bin/notifier ./cmd/notifier
    entrypoint: bin/notifier
on:
  - status_changed
timeout: 30s
`, name)
}

func readDoc(t *testing.T, path string) *manifest.RegistryDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc manifest.RegistryDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return &doc
}

func pluginNames(entries []manifest.RegistryEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}
