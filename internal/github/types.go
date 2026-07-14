// Package github talks to the GitHub REST API. It handles topic search,
// repository metadata, raw file contents, and release / commit resolution.
// The types here are the crawler's view of GitHub — we do not expose the
// full API response shape; anything the crawler does not consult stays
// unexported.
package github

import "time"

// Repo is a repository row returned from topic search. Fields match the
// subset the crawler needs: filters (fork / archived / private), name
// collision tiebreak (CreatedAt), and where to look for a manifest
// (DefaultBranch).
type Repo struct {
	FullName      string
	Description   string
	Homepage      string
	Fork          bool
	Archived      bool
	Private       bool
	CreatedAt     time.Time
	DefaultBranch string
	License       string
}

// Version is one resolved plugin version — either a release tag with its
// pinned commit, or the default-branch HEAD when a repository ships no
// releases.
type Version struct {
	// Version is the semver ("0.3.1"), stripped of any leading "v".
	Version string
	// Tag is the git tag as-is ("v0.3.1"), or "HEAD" for default-branch
	// resolution. The registry entry stores it verbatim so `jin plugin
	// install` can print it in the consent screen.
	Tag string
	// SHA is the commit that Tag points at. It is the decisive install key.
	SHA string
	// CommittedAt is the commit time — used for RegistryEntry.UpdatedAt on
	// the newest version.
	CommittedAt time.Time
}
