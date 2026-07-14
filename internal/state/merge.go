package state

import (
	"sort"
	"time"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// Merge produces the next registry document from the previous one and the
// current crawl. now is the crawl-start time (also written into
// GeneratedAt); grace is how long an orphaned entry survives without a live
// owner.
//
// The merge is a pure function: no I/O, no hidden clock. All decisions
// spring from prev + cur + now + grace, which is why the collision rules
// can be exhaustively exercised by tests.
//
// Returned along with the document is a list of claims that were rejected
// (ignored due to prior ownership or created_at tiebreak). The crawler logs
// them as WARNings so publishers can see why their claim did not land.
func Merge(prev *manifest.RegistryDocument, cur *CrawlResult, now time.Time, grace time.Duration) (*manifest.RegistryDocument, []IgnoredClaim) {
	if prev == nil {
		prev = emptyDoc()
	}
	if cur == nil {
		cur = &CrawlResult{}
	}
	if grace <= 0 {
		grace = DefaultGracePeriod
	}

	out := &manifest.RegistryDocument{
		SchemaVersion: manifest.CurrentSchemaVersion,
		GeneratedAt:   now,
		Plugins:       []manifest.RegistryEntry{},
	}
	handled := map[string]bool{}
	var ignored []IgnoredClaim

	// Phase 1 — carry over prev entries. Owner-continuity is the strongest
	// rule and must be applied before any new claim gets a chance.
	for _, prevEntry := range prev.Plugins {
		name := prevEntry.Name
		claims := cur.ByName[name]
		ownerClaim, otherClaims := splitByRepo(claims, prevEntry.Repo)

		// Rule: prev owner is present → continue, refresh versions, clear orphan.
		if ownerClaim != nil {
			carried := applyClaim(prevEntry, *ownerClaim)
			carried.OrphanedSince = nil
			out.Plugins = append(out.Plugins, carried)
			for _, c := range otherClaims {
				ignored = append(ignored, IgnoredClaim{
					Claim:  c,
					Reason: "name already owned by " + prevEntry.Repo,
				})
			}
			handled[name] = true
			continue
		}

		// Rule: prev owner is absent → orphaned flow.
		orphanedSince := now
		if prevEntry.OrphanedSince != nil {
			orphanedSince = *prevEntry.OrphanedSince
		}
		if now.Sub(orphanedSince) >= grace {
			// Grace ended — release the name. Do NOT set handled[name] so any
			// current claims fall through to the new-name path below and can
			// take ownership in the same run.
			continue
		}
		// Still within grace → keep the (orphaned) entry and reject other
		// repos that tried to claim the name during grace.
		carried := prevEntry
		carried.OrphanedSince = &orphanedSince
		out.Plugins = append(out.Plugins, carried)
		for _, c := range otherClaims {
			ignored = append(ignored, IgnoredClaim{
				Claim:  c,
				Reason: "name orphaned by " + prevEntry.Repo + " and still within grace period",
			})
		}
		handled[name] = true
	}

	// Phase 2 — new-name arbitration for anything prev did not decide.
	var newNames []string
	for name := range cur.ByName {
		if !handled[name] {
			newNames = append(newNames, name)
		}
	}
	sort.Strings(newNames)

	for _, name := range newNames {
		claims := cur.ByName[name]
		chosen, rejected := chooseByCreatedAt(claims)
		entry := manifest.RegistryEntry{
			Name:           name,
			Repo:           chosen.Repo,
			FirstClaimedAt: now,
		}
		entry = applyClaim(entry, chosen)
		out.Plugins = append(out.Plugins, entry)
		for _, c := range rejected {
			ignored = append(ignored, IgnoredClaim{
				Claim:  c,
				Reason: "name awarded to " + chosen.Repo + " by created_at tiebreak",
			})
		}
	}

	sort.Slice(out.Plugins, func(i, j int) bool {
		return out.Plugins[i].Name < out.Plugins[j].Name
	})
	return out, ignored
}

// splitByRepo scans claims for one whose Repo matches target. It returns a
// pointer to that claim (or nil) plus all remaining claims.
func splitByRepo(claims []Claim, target string) (*Claim, []Claim) {
	var owner *Claim
	var others []Claim
	for i := range claims {
		if owner == nil && claims[i].Repo == target {
			owner = &claims[i]
			continue
		}
		others = append(others, claims[i])
	}
	return owner, others
}

// chooseByCreatedAt implements the "earlier repo wins" tiebreak. When two
// repos have identical timestamps, we further break by Repo string
// (deterministic and audit-friendly) so the result never depends on map
// iteration order.
func chooseByCreatedAt(claims []Claim) (Claim, []Claim) {
	sorted := append([]Claim(nil), claims...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].RepoCreatedAt.Equal(sorted[j].RepoCreatedAt) {
			return sorted[i].RepoCreatedAt.Before(sorted[j].RepoCreatedAt)
		}
		return sorted[i].Repo < sorted[j].Repo
	})
	return sorted[0], sorted[1:]
}

// applyClaim projects a Claim onto an entry, preserving fields the entry
// already owned (Name, Repo, FirstClaimedAt) and refreshing everything the
// manifest and version resolution decide (description, license, versions,
// updated_at). OrphanedSince is left untouched; the caller sets it.
func applyClaim(entry manifest.RegistryEntry, c Claim) manifest.RegistryEntry {
	m := c.Manifest
	entry.Description = m.Description
	entry.Homepage = m.Homepage
	entry.License = m.License
	entry.JinCompat = m.Jin
	entry.LatestVersion = c.LatestVersion
	entry.Versions = c.Versions
	entry.UpdatedAt = c.UpdatedAt
	return entry
}
