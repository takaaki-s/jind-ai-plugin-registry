package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, mux http.Handler) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(Config{
		Token:      "test-token",
		BaseURL:    srv.URL,
		RawBaseURL: srv.URL + "/raw",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return c
}

func TestSearchByTopic_Pagination(t *testing.T) {
	// Page 1 returns 100 repos, page 2 returns 50 — the client must
	// keep paging until a page comes back short.
	mux := http.NewServeMux()
	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		page := r.URL.Query().Get("page")
		var items []map[string]any
		switch page {
		case "1":
			for i := 0; i < 100; i++ {
				items = append(items, map[string]any{
					"full_name":      fmt.Sprintf("owner/repo-%d", i),
					"default_branch": "main",
					"created_at":     "2025-01-01T00:00:00Z",
				})
			}
		case "2":
			for i := 0; i < 50; i++ {
				items = append(items, map[string]any{
					"full_name":      fmt.Sprintf("owner/repo-%d", 100+i),
					"default_branch": "main",
					"created_at":     "2025-01-01T00:00:00Z",
				})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})

	c := newTestClient(t, mux)
	got, err := c.SearchByTopic(context.Background(), "jind-ai-plugin")
	if err != nil {
		t.Fatalf("SearchByTopic: %v", err)
	}
	if len(got) != 150 {
		t.Fatalf("want 150 repos across two pages, got %d", len(got))
	}
	if got[0].FullName != "owner/repo-0" || got[149].FullName != "owner/repo-149" {
		t.Errorf("pagination order broke: first=%s last=%s", got[0].FullName, got[149].FullName)
	}
}

func TestSearchByTopic_FiltersAndLicense(t *testing.T) {
	// A single-item response with fork+archived+license — verifies field
	// mapping since the crawler filters by these flags downstream.
	mux := http.NewServeMux()
	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{
				"full_name":      "foo/bar",
				"description":    "hello",
				"homepage":       "https://example.com",
				"fork":           true,
				"archived":       true,
				"private":        false,
				"created_at":     "2025-06-01T00:00:00Z",
				"default_branch": "trunk",
				"license":        map[string]any{"spdx_id": "MIT"},
			}},
		})
	})
	c := newTestClient(t, mux)
	got, err := c.SearchByTopic(context.Background(), "jind-ai-plugin")
	if err != nil {
		t.Fatalf("SearchByTopic: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 repo, got %d", len(got))
	}
	r := got[0]
	if !r.Fork || !r.Archived || r.Private {
		t.Errorf("filter flags mis-parsed: fork=%v archived=%v private=%v", r.Fork, r.Archived, r.Private)
	}
	if r.License != "MIT" {
		t.Errorf("license mis-parsed: %q", r.License)
	}
	if r.DefaultBranch != "trunk" {
		t.Errorf("default_branch mis-parsed: %q", r.DefaultBranch)
	}
}

func TestGetManifest_OK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/raw/foo/bar/abc123/jind-ai-plugin.yaml", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("name: foo\n"))
	})
	c := newTestClient(t, mux)
	body, err := c.GetManifest(context.Background(), "foo/bar", "abc123", "jind-ai-plugin.yaml")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if string(body) != "name: foo\n" {
		t.Errorf("body mismatch: %q", body)
	}
}

func TestGetManifest_Missing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/raw/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	c := newTestClient(t, mux)
	_, err := c.GetManifest(context.Background(), "foo/bar", "main", "jind-ai-plugin.yaml")
	if !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("want ErrManifestMissing, got %v", err)
	}
}

func TestListVersions_TakesNewestSemverAndSkipsPrerelease(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/bar/releases", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v0.9.0", "draft": false, "prerelease": false},
			{"tag_name": "v1.0.0-rc.1", "draft": false, "prerelease": true},  // skip
			{"tag_name": "not-semver", "draft": false, "prerelease": false},  // skip
			{"tag_name": "v1.1.0", "draft": false, "prerelease": false},
			{"tag_name": "v1.0.0", "draft": false, "prerelease": false},
			{"tag_name": "v0.8.0", "draft": false, "prerelease": false},      // will be truncated (cap=3)
			{"tag_name": "v0.7.0", "draft": true, "prerelease": false},       // skip
		})
	})
	commitTimes := map[string]string{
		"v1.1.0": "2026-07-01T00:00:00Z",
		"v1.0.0": "2026-06-01T00:00:00Z",
		"v0.9.0": "2026-05-01T00:00:00Z",
	}
	mux.HandleFunc("/repos/foo/bar/commits/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/repos/foo/bar/commits/")
		date, ok := commitTimes[tag]
		if !ok {
			http.Error(w, "unexpected tag "+tag, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha": "sha-" + tag,
			"commit": map[string]any{
				"committer": map[string]any{"date": date},
			},
		})
	})
	c := newTestClient(t, mux)
	versions, err := c.ListVersions(context.Background(), "foo/bar", "main")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("want 3 versions (cap), got %d", len(versions))
	}
	want := []string{"1.1.0", "1.0.0", "0.9.0"}
	for i, w := range want {
		if versions[i].Version != w {
			t.Errorf("versions[%d].Version = %s, want %s", i, versions[i].Version, w)
		}
		if versions[i].SHA != "sha-v"+w {
			t.Errorf("versions[%d].SHA = %s, want sha-v%s", i, versions[i].SHA, w)
		}
	}
}

func TestListVersions_FallsBackToDefaultBranchHead(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/bar/releases", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{}) // no releases
	})
	mux.HandleFunc("/repos/foo/bar/commits/", func(w http.ResponseWriter, r *http.Request) {
		branch := strings.TrimPrefix(r.URL.Path, "/repos/foo/bar/commits/")
		if branch != "main" {
			http.Error(w, "unexpected branch "+branch, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha":    "head-sha",
			"commit": map[string]any{"committer": map[string]any{"date": "2026-07-14T00:00:00Z"}},
		})
	})
	c := newTestClient(t, mux)
	versions, err := c.ListVersions(context.Background(), "foo/bar", "main")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("want single HEAD version, got %d", len(versions))
	}
	if versions[0].SHA != "head-sha" || versions[0].Tag != "HEAD" || versions[0].Version != "0.0.0" {
		t.Errorf("HEAD fallback wrong: %+v", versions[0])
	}
}

func TestNewHTTPClient_TokenRequired(t *testing.T) {
	_, err := NewHTTPClient(Config{})
	if err == nil {
		t.Fatal("want error for missing token")
	}
}
