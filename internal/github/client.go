package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

// DefaultBaseURL is api.github.com. Tests inject an httptest.Server URL via
// Config.BaseURL.
const DefaultBaseURL = "https://api.github.com"

// DefaultRawBaseURL is the raw.githubusercontent.com endpoint used to fetch
// manifest file contents. Split from BaseURL because the two hosts have
// different rate limits and mocking is cleaner when each is a distinct URL.
const DefaultRawBaseURL = "https://raw.githubusercontent.com"

// Config configures HTTPClient. Zero values apply sensible defaults except
// for Token, which is required — the crawler always runs authenticated to
// avoid the 60-req/h unauthenticated ceiling.
type Config struct {
	// Token is a GitHub PAT or Actions token. Required.
	Token string
	// BaseURL overrides api.github.com; empty uses DefaultBaseURL.
	BaseURL string
	// RawBaseURL overrides raw.githubusercontent.com; empty uses
	// DefaultRawBaseURL.
	RawBaseURL string
	// HTTPClient overrides the default (30s timeout). Tests substitute
	// httptest.Server.Client() so its TLS/timeout matches.
	HTTPClient *http.Client
}

// HTTPClient talks to the real GitHub API. It is the crawler's only HTTP
// dependency — every other package speaks pure Go.
type HTTPClient struct {
	token      string
	base       string
	raw        string
	httpClient *http.Client
}

// NewHTTPClient validates cfg and returns a ready client.
func NewHTTPClient(cfg Config) (*HTTPClient, error) {
	if cfg.Token == "" {
		return nil, errors.New("github: token is required")
	}
	c := &HTTPClient{
		token:      cfg.Token,
		base:       cfg.BaseURL,
		raw:        cfg.RawBaseURL,
		httpClient: cfg.HTTPClient,
	}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	if c.raw == "" {
		c.raw = DefaultRawBaseURL
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return c, nil
}

// SearchByTopic pages through the search API and returns every repository
// tagged with the given topic. Repos are returned in whatever order the
// GitHub search API produces — the crawler must not rely on order for
// correctness (name collisions are settled by CreatedAt, not by iteration
// order).
func (c *HTTPClient) SearchByTopic(ctx context.Context, topic string) ([]Repo, error) {
	var out []Repo
	page := 1
	for {
		u := c.base + "/search/repositories?" + url.Values{
			"q":        []string{"topic:" + topic},
			"per_page": []string{"100"},
			"page":     []string{fmt.Sprintf("%d", page)},
		}.Encode()

		var resp struct {
			TotalCount int          `json:"total_count"`
			Items      []searchItem `json:"items"`
		}
		if err := c.getJSON(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("search topic %q page %d: %w", topic, page, err)
		}
		if out == nil && resp.TotalCount > 0 {
			// GitHub search API caps at 1000 results regardless of TotalCount.
			n := resp.TotalCount
			if n > 1000 {
				n = 1000
			}
			out = make([]Repo, 0, n)
		}
		for _, it := range resp.Items {
			out = append(out, it.toRepo())
		}
		if len(resp.Items) < 100 {
			break
		}
		page++
	}
	return out, nil
}

// RawURL is the canonical raw.githubusercontent.com URL for a SHA-pinned
// file under this client's configured RawBaseURL. Callers use it to
// populate RegistryVersion.ManifestURL so a mocked RawBaseURL (tests,
// staging mirrors) does not leak the production host into registry.json.
func (c *HTTPClient) RawURL(repo, sha, path string) string {
	return fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(c.raw, "/"), repo, sha, path)
}

// GetManifest fetches the raw jind-ai-plugin.yaml at the given ref. ref may
// be a branch name, a tag, or a commit SHA — raw.githubusercontent.com
// resolves all three. A missing file surfaces as ErrManifestMissing so the
// crawler can log-and-skip without treating it as a hard failure.
func (c *HTTPClient) GetManifest(ctx context.Context, repo, ref, path string) ([]byte, error) {
	u := fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(c.raw, "/"), repo, ref, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrManifestMissing
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("raw content HTTP %d for %s@%s", resp.StatusCode, repo, ref)
	}
	return io.ReadAll(resp.Body)
}

// ErrManifestMissing signals "the repo does not contain a manifest at the
// requested ref/path". Crawler treats this as a skip, not a hard fail.
var ErrManifestMissing = errors.New("manifest missing")

// ListVersions returns every resolvable stable release, newest first, or a
// single HEAD entry when the repository ships no releases. Non-semver,
// draft, and prerelease tags are skipped. Callers are responsible for
// capping the list to the number the registry wants to publish — the
// github package is deliberately unaware of that policy.
func (c *HTTPClient) ListVersions(ctx context.Context, repo, defaultBranch string) ([]Version, error) {
	releases, err := c.listReleases(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("list releases %s: %w", repo, err)
	}
	if len(releases) > 0 {
		versions, err := c.resolveReleases(ctx, repo, releases)
		if err != nil {
			return nil, err
		}
		if len(versions) > 0 {
			return versions, nil
		}
	}
	// Fall back to default-branch HEAD.
	commit, err := c.commitOf(ctx, repo, defaultBranch)
	if err != nil {
		return nil, fmt.Errorf("resolve default branch %s of %s: %w", defaultBranch, repo, err)
	}
	return []Version{{
		Version:     "0.0.0",
		Tag:         "HEAD",
		SHA:         commit.SHA,
		CommittedAt: commit.CommittedAt,
	}}, nil
}

func (c *HTTPClient) listReleases(ctx context.Context, repo string) ([]release, error) {
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=100", c.base, repo)
	var out []release
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// resolveReleases turns raw releases into Version entries sorted
// newest-first by semver. Drafts, prereleases, and non-semver tags are
// dropped. The registry-side truncation (top-N) is applied by the caller,
// not here.
func (c *HTTPClient) resolveReleases(ctx context.Context, repo string, releases []release) ([]Version, error) {
	type parsed struct {
		release release
		ver     *semver.Version
	}
	var kept []parsed
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		v, err := semver.NewVersion(r.TagName)
		if err != nil {
			continue
		}
		kept = append(kept, parsed{release: r, ver: v})
	}
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].ver.GreaterThan(kept[j].ver)
	})

	out := make([]Version, 0, len(kept))
	for _, p := range kept {
		// target_commitish is often a branch name; the reliable commit SHA
		// comes from the commits endpoint keyed on the tag itself.
		commit, err := c.commitOf(ctx, repo, p.release.TagName)
		if err != nil {
			return nil, fmt.Errorf("resolve tag %s on %s: %w", p.release.TagName, repo, err)
		}
		out = append(out, Version{
			Version:     p.ver.String(),
			Tag:         p.release.TagName,
			SHA:         commit.SHA,
			CommittedAt: commit.CommittedAt,
		})
	}
	return out, nil
}

// commit is what the crawler actually needs from GitHub's commit endpoint:
// the SHA the ref resolved to, and the commit time.
type commit struct {
	SHA         string
	CommittedAt time.Time
}

// commitOf resolves a ref (tag or branch name) to its commit SHA and
// commit time. The commits/{ref} endpoint accepts both and returns the
// commit metadata in a single round trip.
func (c *HTTPClient) commitOf(ctx context.Context, repo, ref string) (*commit, error) {
	u := fmt.Sprintf("%s/repos/%s/commits/%s", c.base, repo, url.PathEscape(ref))
	var resp struct {
		SHA    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	if resp.SHA == "" {
		return nil, fmt.Errorf("commit lookup returned empty sha for %s@%s", repo, ref)
	}
	return &commit{SHA: resp.SHA, CommittedAt: resp.Commit.Committer.Date}, nil
}

// getJSON is the shared retry-free JSON GET. All auth headers, error
// mapping, and response decoding live here so per-endpoint methods stay
// focused on shaping the URL and destination struct.
func (c *HTTPClient) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// searchItem is the on-the-wire shape of one repository entry from the
// search API. Fields the crawler does not read are omitted.
type searchItem struct {
	FullName      string    `json:"full_name"`
	Description   string    `json:"description"`
	Homepage      string    `json:"homepage"`
	Fork          bool      `json:"fork"`
	Archived      bool      `json:"archived"`
	Private       bool      `json:"private"`
	CreatedAt     time.Time `json:"created_at"`
	DefaultBranch string    `json:"default_branch"`
	License       *struct {
		SPDXID string `json:"spdx_id"`
	} `json:"license"`
}

func (s searchItem) toRepo() Repo {
	r := Repo{
		FullName:      s.FullName,
		Description:   s.Description,
		Homepage:      s.Homepage,
		Fork:          s.Fork,
		Archived:      s.Archived,
		Private:       s.Private,
		CreatedAt:     s.CreatedAt,
		DefaultBranch: s.DefaultBranch,
	}
	if s.License != nil {
		r.License = s.License.SPDXID
	}
	return r
}

// release is the subset of the releases-API response the crawler reads.
type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}
