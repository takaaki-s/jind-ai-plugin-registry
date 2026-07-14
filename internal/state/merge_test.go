package state

import (
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// The seven collision / orphan branches listed in .tasks/plugin-registry-mvp/
// 03_crawler.md are each covered as a separate subtest so a failure isolates
// the rule immediately.

// mkClaim returns a Claim wired with the mandatory fields tests need. The
// helper keeps every test-body focused on the specific inputs that matter to
// its branch (repo, created_at, name, versions).
func mkClaim(repo, name, ver string, created time.Time) Claim {
	return Claim{
		Repo:          repo,
		RepoCreatedAt: created,
		Manifest: &manifest.Manifest{
			SchemaVersion: 1,
			Name:          name,
			Version:       ver,
			Description:   "test " + name,
			License:       "MIT",
			Homepage:      "https://example.com/" + name,
			Jin:           ">=0.7.0",
		},
		LatestVersion: ver,
		Versions: []manifest.RegistryVersion{{
			Version:     ver,
			SHA:         "sha-" + ver,
			ManifestURL: "https://example.com/manifest.yaml",
			Tag:         "v" + ver,
		}},
		UpdatedAt: created.Add(24 * time.Hour),
	}
}

// mkResult wraps AddClaim so tests can build a CrawlResult in one expression.
func mkResult(claims ...Claim) *CrawlResult {
	r := &CrawlResult{}
	for _, c := range claims {
		r.AddClaim(c)
	}
	return r
}

// prevEntry constructs a previous-registry entry with the fields the merge
// logic actually reads (Name, Repo, FirstClaimedAt, OrphanedSince).
func prevEntry(name, repo string, firstClaimed time.Time, orphanedSince *time.Time) manifest.RegistryEntry {
	return manifest.RegistryEntry{
		Name:           name,
		Repo:           repo,
		Description:    "stale " + name,
		JinCompat:      ">=0.6.0",
		LatestVersion:  "0.1.0",
		Versions:       []manifest.RegistryVersion{{Version: "0.1.0", SHA: "old-sha", Tag: "v0.1.0"}},
		FirstClaimedAt: firstClaimed,
		OrphanedSince:  orphanedSince,
		UpdatedAt:      firstClaimed,
	}
}

func TestMerge_PrevOwnerContinues(t *testing.T) {
	prev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{
			prevEntry("foo", "alice/foo", mustParse("2026-01-01T00:00:00Z"), nil),
		},
	}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := mkResult(mkClaim("alice/foo", "foo", "0.2.0", mustParse("2025-06-01T00:00:00Z")))

	got, ignored := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 {
		t.Fatalf("want 1 plugin, got %d", len(got.Plugins))
	}
	entry := got.Plugins[0]
	if entry.Repo != "alice/foo" {
		t.Errorf("owner drifted: %s", entry.Repo)
	}
	if entry.LatestVersion != "0.2.0" {
		t.Errorf("versions not refreshed: %s", entry.LatestVersion)
	}
	if entry.Description != "test foo" {
		t.Errorf("description not refreshed: %q", entry.Description)
	}
	if entry.OrphanedSince != nil {
		t.Errorf("orphan flag should have been cleared, got %v", entry.OrphanedSince)
	}
	if !entry.FirstClaimedAt.Equal(mustParse("2026-01-01T00:00:00Z")) {
		t.Errorf("first_claimed_at should be immutable, got %v", entry.FirstClaimedAt)
	}
	if len(ignored) != 0 {
		t.Errorf("no claims should be ignored, got %d", len(ignored))
	}
}

func TestMerge_PrevOwnerDefeatsRivalClaim(t *testing.T) {
	prev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{
			prevEntry("foo", "alice/foo", mustParse("2026-01-01T00:00:00Z"), nil),
		},
	}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := mkResult(
		mkClaim("alice/foo", "foo", "0.2.0", mustParse("2025-06-01T00:00:00Z")),
		mkClaim("mallory/foo", "foo", "9.9.9", mustParse("2024-01-01T00:00:00Z")), // older repo, tries to steal
	)

	got, ignored := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 || got.Plugins[0].Repo != "alice/foo" {
		t.Fatalf("prev owner must win, got plugins=%+v", got.Plugins)
	}
	if len(ignored) != 1 || ignored[0].Claim.Repo != "mallory/foo" {
		t.Fatalf("mallory's claim should be ignored, got %+v", ignored)
	}
}

func TestMerge_NewName_SingleRepo(t *testing.T) {
	prev := &manifest.RegistryDocument{SchemaVersion: 1}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := mkResult(mkClaim("bob/bar", "bar", "0.1.0", mustParse("2025-01-01T00:00:00Z")))

	got, ignored := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 || got.Plugins[0].Repo != "bob/bar" {
		t.Fatalf("single claim must win, got %+v", got.Plugins)
	}
	if !got.Plugins[0].FirstClaimedAt.Equal(now) {
		t.Errorf("first_claimed_at should equal now for a new name, got %v", got.Plugins[0].FirstClaimedAt)
	}
	if len(ignored) != 0 {
		t.Errorf("no ignored claims expected, got %d", len(ignored))
	}
}

func TestMerge_NewName_MultipleRepos_CreatedAtTiebreak(t *testing.T) {
	prev := &manifest.RegistryDocument{SchemaVersion: 1}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := mkResult(
		mkClaim("newcomer/baz", "baz", "0.1.0", mustParse("2026-06-01T00:00:00Z")),
		mkClaim("veteran/baz", "baz", "0.1.0", mustParse("2020-01-01T00:00:00Z")),
	)

	got, ignored := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 || got.Plugins[0].Repo != "veteran/baz" {
		t.Fatalf("older repo must win, got %+v", got.Plugins)
	}
	if len(ignored) != 1 || ignored[0].Claim.Repo != "newcomer/baz" {
		t.Fatalf("newcomer's claim should be ignored, got %+v", ignored)
	}
}

func TestMerge_TransitionToOrphaned(t *testing.T) {
	prev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{
			prevEntry("foo", "alice/foo", mustParse("2026-01-01T00:00:00Z"), nil),
		},
	}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := &CrawlResult{} // alice/foo did not appear this run

	got, _ := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 {
		t.Fatalf("orphan entry must be preserved, got %d plugins", len(got.Plugins))
	}
	entry := got.Plugins[0]
	if entry.OrphanedSince == nil || !entry.OrphanedSince.Equal(now) {
		t.Errorf("orphaned_since should be stamped to now, got %v", entry.OrphanedSince)
	}
}

func TestMerge_Grace_Revive(t *testing.T) {
	orphaned := mustParse("2026-07-01T00:00:00Z")
	prev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{
			prevEntry("foo", "alice/foo", mustParse("2026-01-01T00:00:00Z"), &orphaned),
		},
	}
	now := mustParse("2026-07-14T00:00:00Z") // still within 30d grace
	cur := mkResult(mkClaim("alice/foo", "foo", "0.3.0", mustParse("2025-06-01T00:00:00Z")))

	got, _ := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 || got.Plugins[0].Repo != "alice/foo" {
		t.Fatalf("owner must reclaim its name after revive, got %+v", got.Plugins)
	}
	if got.Plugins[0].OrphanedSince != nil {
		t.Errorf("orphan flag should have been cleared on revive, got %v", got.Plugins[0].OrphanedSince)
	}
	if got.Plugins[0].LatestVersion != "0.3.0" {
		t.Errorf("versions should be refreshed on revive, got %s", got.Plugins[0].LatestVersion)
	}
}

func TestMerge_Grace_HoldsAgainstRivalDuringOrphaned(t *testing.T) {
	orphaned := mustParse("2026-07-01T00:00:00Z")
	prev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{
			prevEntry("foo", "alice/foo", mustParse("2026-01-01T00:00:00Z"), &orphaned),
		},
	}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := mkResult(mkClaim("carol/foo", "foo", "0.1.0", mustParse("2020-01-01T00:00:00Z"))) // very old rival

	got, ignored := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 || got.Plugins[0].Repo != "alice/foo" {
		t.Fatalf("orphaned owner still holds the name during grace, got %+v", got.Plugins)
	}
	if got.Plugins[0].OrphanedSince == nil {
		t.Errorf("orphan flag must persist during grace")
	}
	if len(ignored) != 1 || ignored[0].Claim.Repo != "carol/foo" {
		t.Fatalf("rival claim should be ignored during grace, got %+v", ignored)
	}
}

func TestMerge_Grace_Expired_FreesName(t *testing.T) {
	orphaned := mustParse("2026-05-01T00:00:00Z")
	prev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{
			prevEntry("foo", "alice/foo", mustParse("2026-01-01T00:00:00Z"), &orphaned),
		},
	}
	now := mustParse("2026-07-14T00:00:00Z") // 74 days later, well past 30d grace
	cur := &CrawlResult{}                    // no one claims it

	got, _ := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 0 {
		t.Fatalf("expired orphan must be removed, got %+v", got.Plugins)
	}
}

// Extra: the trickiest interaction — grace ended AND a new claimant is
// present in the same run. The new claimant should take the freed name.
func TestMerge_Grace_Expired_NewClaimantTakesName(t *testing.T) {
	orphaned := mustParse("2026-05-01T00:00:00Z")
	prev := &manifest.RegistryDocument{
		SchemaVersion: 1,
		Plugins: []manifest.RegistryEntry{
			prevEntry("foo", "alice/foo", mustParse("2026-01-01T00:00:00Z"), &orphaned),
		},
	}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := mkResult(mkClaim("dave/foo", "foo", "1.0.0", mustParse("2025-01-01T00:00:00Z")))

	got, _ := Merge(prev, cur, now, DefaultGracePeriod)

	if len(got.Plugins) != 1 || got.Plugins[0].Repo != "dave/foo" {
		t.Fatalf("new claimant must own the freed name, got %+v", got.Plugins)
	}
	if !got.Plugins[0].FirstClaimedAt.Equal(now) {
		t.Errorf("first_claimed_at should be reset when name is re-claimed, got %v", got.Plugins[0].FirstClaimedAt)
	}
	if got.Plugins[0].OrphanedSince != nil {
		t.Errorf("orphan flag should be nil on a freshly claimed name")
	}
}

// Sort determinism — Merge output is name-sorted so registry.json diffs are
// reviewable and cache validators (ETag) do not thrash on map iteration
// order.
func TestMerge_OutputSortedByName(t *testing.T) {
	prev := &manifest.RegistryDocument{SchemaVersion: 1}
	now := mustParse("2026-07-14T00:00:00Z")
	cur := mkResult(
		mkClaim("owner/z", "zeta", "0.1.0", now),
		mkClaim("owner/a", "alpha", "0.1.0", now),
		mkClaim("owner/m", "mu", "0.1.0", now),
	)
	got, _ := Merge(prev, cur, now, DefaultGracePeriod)
	if len(got.Plugins) != 3 {
		t.Fatalf("want 3 plugins, got %d", len(got.Plugins))
	}
	if got.Plugins[0].Name != "alpha" || got.Plugins[1].Name != "mu" || got.Plugins[2].Name != "zeta" {
		t.Errorf("plugins not alphabetically sorted: %v", []string{
			got.Plugins[0].Name, got.Plugins[1].Name, got.Plugins[2].Name,
		})
	}
}

// Same created_at → deterministic tiebreak on repo string.
func TestMerge_NewName_CreatedAtTie_RepoStringBreaks(t *testing.T) {
	prev := &manifest.RegistryDocument{SchemaVersion: 1}
	now := mustParse("2026-07-14T00:00:00Z")
	created := mustParse("2025-01-01T00:00:00Z")
	cur := mkResult(
		mkClaim("z-owner/thing", "thing", "0.1.0", created),
		mkClaim("a-owner/thing", "thing", "0.1.0", created),
	)
	got, _ := Merge(prev, cur, now, DefaultGracePeriod)
	if len(got.Plugins) != 1 || got.Plugins[0].Repo != "a-owner/thing" {
		t.Fatalf("lexicographic tiebreak failed: %+v", got.Plugins)
	}
}

func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
