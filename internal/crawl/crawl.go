// Package crawl orchestrates one crawler run: load prev registry, search
// GitHub for tagged repos, resolve each into a Claim, merge, and write the
// new registry. All GitHub I/O happens through the injected GitHubClient
// interface so integration tests can point Run at an httptest.Server.
package crawl

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/takaaki-s/jind-ai-plugin-registry/internal/github"
	"github.com/takaaki-s/jind-ai-plugin-registry/internal/output"
	"github.com/takaaki-s/jind-ai-plugin-registry/internal/state"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// DefaultTopic is the discovery topic every publisher tags their repo with.
const DefaultTopic = "jind-ai-plugin"

// versionCap is the number of newest releases the crawler records per
// plugin. Three is enough for install-latest, rollback, and compat
// inspection without letting registry.json grow unboundedly (02_registry.md).
// The cap lives here — not in the github client — because it is a
// registry-emit policy, not a fact about GitHub.
const versionCap = 3

// GitHubClient is the crawler's view of the GitHub API. Consumer-defined so
// tests can substitute an in-memory fake and the real implementation lives
// in a package the crawler does not import back.
type GitHubClient interface {
	SearchByTopic(ctx context.Context, topic string) ([]github.Repo, error)
	GetManifest(ctx context.Context, repo, ref, path string) ([]byte, error)
	ListVersions(ctx context.Context, repo, defaultBranch string) ([]github.Version, error)
	// RawURL returns the raw file URL for a SHA-pinned path. Sourcing this
	// from the client (rather than hardcoding raw.githubusercontent.com)
	// lets a mocked RawBaseURL propagate to RegistryVersion.ManifestURL.
	RawURL(repo, sha, path string) string
}

// Options configures one Run. Zero values apply sensible defaults except
// Client, PrevPath, and OutPath, which are always required.
type Options struct {
	Client   GitHubClient
	PrevPath string
	OutPath  string
	// Topic overrides DefaultTopic (used in tests).
	Topic string
	// Now returns the crawl start time. Nil uses time.Now.
	Now func() time.Time
	// Grace overrides state.DefaultGracePeriod.
	Grace time.Duration
	// Logger receives WARN / INFO lines. Nil uses log.Default().
	Logger *log.Logger
}

// Run executes one crawler pass. It is the single entry point used by both
// cmd/crawler/main.go and the integration test — no orchestration logic
// lives in main().
func Run(ctx context.Context, opts Options) error {
	if opts.Client == nil {
		return errors.New("crawl: Client is required")
	}
	if opts.PrevPath == "" || opts.OutPath == "" {
		return errors.New("crawl: PrevPath and OutPath are required")
	}
	topic := opts.Topic
	if topic == "" {
		topic = DefaultTopic
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Step 1 — Prev registry is state. Failure here is fail-close because
	// crawling without prev state can non-deterministically flip ownership.
	prev, err := state.LoadPrev(opts.PrevPath)
	if err != nil {
		return fmt.Errorf("load prev registry: %w", err)
	}

	// Step 2 — Search.
	repos, err := opts.Client.SearchByTopic(ctx, topic)
	if err != nil {
		return fmt.Errorf("search topic %q: %w", topic, err)
	}
	logger.Printf("crawl: search returned %d repos for topic %q", len(repos), topic)

	// Steps 3-5 — Filter, fetch manifest, resolve versions.
	cur := &state.CrawlResult{}
	for _, repo := range repos {
		if reason := skipReason(repo); reason != "" {
			logger.Printf("crawl: skip %s (%s)", repo.FullName, reason)
			continue
		}
		claim, err := buildClaim(ctx, opts.Client, repo)
		if err != nil {
			logger.Printf("crawl: skip %s (%v)", repo.FullName, err)
			continue
		}
		cur.AddClaim(*claim)
	}

	// Step 6-7 — Merge with prev registry; Merge also handles orphaned flow.
	nextDoc, ignored := state.Merge(prev, cur, now(), opts.Grace)
	for _, ig := range ignored {
		logger.Printf("crawl: ignore claim from %s: %s", ig.Claim.Repo, ig.Reason)
	}

	// Step 8 — Write.
	if err := output.Write(opts.OutPath, nextDoc); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	logger.Printf("crawl: wrote %s with %d plugins", opts.OutPath, len(nextDoc.Plugins))
	return nil
}

// skipReason encodes the pre-processing filters from 03_crawler.md step 3.
// It returns a human-readable reason when the repo must be skipped, or ""
// when the repo should proceed to manifest fetch.
func skipReason(r github.Repo) string {
	switch {
	case r.Fork:
		return "fork"
	case r.Private:
		return "private"
	case r.Archived:
		return "archived"
	case r.DefaultBranch == "":
		return "no default branch"
	}
	return ""
}

// buildClaim resolves versions for the repo, fetches the manifest at the
// newest version's SHA, validates it, and packs the result into a state.Claim.
// Errors here are per-repo skip conditions — buildClaim never fails Run;
// crawl.Run turns any error into a WARN log and moves on.
func buildClaim(ctx context.Context, client GitHubClient, repo github.Repo) (*state.Claim, error) {
	versions, err := client.ListVersions(ctx, repo.FullName, repo.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	if len(versions) == 0 {
		return nil, errors.New("no resolvable versions")
	}
	if len(versions) > versionCap {
		versions = versions[:versionCap]
	}
	newest := versions[0]

	body, err := client.GetManifest(ctx, repo.FullName, newest.SHA, manifest.Filename)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	m, _, err := manifest.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	// Registry is left nil — the uniqueness/ownership rules run through
	// state.Merge instead. Every other manifest-shape rule (name pattern,
	// semver, install XOR, popup bounds, jin range syntax) is exercised here.
	findings := manifest.Check(m, manifest.CheckOptions{})
	if manifest.HasErrors(findings) {
		return nil, fmt.Errorf("manifest invalid: %s", summariseFindings(findings))
	}

	regVersions := make([]manifest.RegistryVersion, 0, len(versions))
	for _, v := range versions {
		regVersions = append(regVersions, manifest.RegistryVersion{
			Version:     v.Version,
			SHA:         v.SHA,
			Tag:         v.Tag,
			ManifestURL: client.RawURL(repo.FullName, v.SHA, manifest.Filename),
		})
	}

	return &state.Claim{
		Repo:          repo.FullName,
		RepoCreatedAt: repo.CreatedAt,
		Manifest:      m,
		LatestVersion: newest.Version,
		Versions:      regVersions,
		UpdatedAt:     newest.CommittedAt,
	}, nil
}

// summariseFindings collapses error-severity findings into a short one-line
// diagnostic. Warnings are dropped because a manifest that only warns still
// installs; the caller only invokes this when HasErrors reported true.
func summariseFindings(findings []manifest.Finding) string {
	var msgs []string
	for _, f := range findings {
		if f.Severity != manifest.SeverityError {
			continue
		}
		msgs = append(msgs, f.String())
	}
	s := strings.Join(msgs, "; ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
