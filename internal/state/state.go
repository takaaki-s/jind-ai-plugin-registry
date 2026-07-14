// Package state holds the pure decision logic that turns a previous
// registry.json plus the results of a fresh crawl into the next
// registry.json. Nothing in this package talks to the network — the GitHub
// side is confined to internal/github, and the file side to internal/output.
// This split makes the collision and orphan rules trivially testable.
package state

import (
	"time"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// DefaultGracePeriod is how long an orphaned name is held before another
// repository may reclaim it. Documented in 03_crawler.md.
const DefaultGracePeriod = 30 * 24 * time.Hour

// Claim is one repository's successful bid to own a plugin name in the
// current crawl. The crawler builds a Claim for every repo that passed
// manifest validation and version resolution.
type Claim struct {
	// Repo is "owner/name" — the GitHub identifier used in RegistryEntry.Repo.
	Repo string
	// RepoCreatedAt is the GitHub-reported creation time. Only consulted when
	// two repos claim the same new name in the same run (tiebreak).
	RepoCreatedAt time.Time
	// Manifest is the parsed jind-ai-plugin.yaml. Description / License /
	// Homepage / Jin flow through to the registry entry.
	Manifest *manifest.Manifest
	// LatestVersion is the semver of the newest version in Versions.
	LatestVersion string
	// Versions is capped at the newest three, ordered newest-first.
	Versions []manifest.RegistryVersion
	// UpdatedAt is the commit time of the newest version.
	UpdatedAt time.Time
}

// CrawlResult is everything the current crawl produced. Names with more than
// one Claim in ByName are same-run collisions the resolver has to arbitrate.
type CrawlResult struct {
	// ByName groups claims by their manifest.Name.
	ByName map[string][]Claim
}

// AddClaim appends a claim under its manifest name. Callers use this rather
// than mutating ByName directly so tests can rely on a stable insertion.
func (r *CrawlResult) AddClaim(c Claim) {
	if r.ByName == nil {
		r.ByName = map[string][]Claim{}
	}
	name := c.Manifest.Name
	r.ByName[name] = append(r.ByName[name], c)
}

// IgnoredClaim is a claim the resolver rejected. The crawler logs these as
// WARNings so an author who publishes a colliding name sees why they were
// not accepted.
type IgnoredClaim struct {
	Claim  Claim
	Reason string
}
