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

// VersionCap is the number of newest releases the crawler records per plugin.
// Kept at three per 02_registry.md — enough for install-latest, rollback,
// and compat inspection without letting registry.json grow unboundedly.
const VersionCap = 3

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

// ListVersions returns the newest N versions per VersionCap. It prefers
// tagged releases (semver-sorted) and falls back to the default-branch
// HEAD when the repository ships no releases. Non-semver tags are skipped.
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
	commit, err := c.headOfBranch(ctx, repo, defaultBranch)
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

// resolveReleases takes the raw releases list and produces up to VersionCap
// Version entries, sorted newest-first by semver. Drafts, prereleases, and
// non-semver tags are skipped — the registry only surfaces stable releases.
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
		v, err := semver.NewVersion(strings.TrimPrefix(r.TagName, "v"))
		if err != nil {
			continue
		}
		kept = append(kept, parsed{release: r, ver: v})
	}
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].ver.GreaterThan(kept[j].ver)
	})
	if len(kept) > VersionCap {
		kept = kept[:VersionCap]
	}

	out := make([]Version, 0, len(kept))
	for _, p := range kept {
		// target_commitish is often a branch name; the reliable commit SHA
		// comes from the git-ref API for the tag.
		commit, err := c.commitForTag(ctx, repo, p.release.TagName)
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

// commitForTag resolves a tag to its underlying commit SHA and commit time.
// It uses the commits endpoint (not git/refs) because commits/{ref} accepts
// tags and returns the commit metadata in one round trip, whereas the git
// ref endpoint can point at an annotated tag object requiring a second call.
func (c *HTTPClient) commitForTag(ctx context.Context, repo, tag string) (*commit, error) {
	return c.commitOf(ctx, repo, tag)
}

// headOfBranch resolves a branch name to its HEAD commit metadata. Same
// endpoint as commitForTag; kept separate for clarity at call sites.
func (c *HTTPClient) headOfBranch(ctx context.Context, repo, branch string) (*commit, error) {
	return c.commitOf(ctx, repo, branch)
}

// commitOf is the shared implementation behind commitForTag and
// headOfBranch — both endpoints resolve `commits/{ref}` where ref is either
// a tag or a branch name.
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
