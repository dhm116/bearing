package fakes

import "time"

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// The fictional org's timeline. Story applies the changes at these times.
var (
	// Start is when the story begins: the first full sync.
	Start = time.Date(2026, 9, 28, 1, 30, 0, 0, time.UTC)
	// RepoRenamedAt is when payments-api is renamed to payments.
	RepoRenamedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// DirectorySyncAt is when the recorded directory feed was read.
	DirectorySyncAt = time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	// CodeownersChangedAt is when payments' CODEOWNERS moves from
	// @acme/payments to @acme/platform.
	CodeownersChangedAt = time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)
	// TeamRenamedAt is when the SRE team is renamed to Reliability.
	TeamRenamedAt = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	// TeamDeletedAt is when the Legacy Ops team is deleted.
	TeamDeletedAt = time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	// MembershipEndsAt is when lfischer's engineering membership ends in
	// the directory, as recorded in advance: nothing changes at this time,
	// the directory simply stops listing it. GitHub never sees it.
	MembershipEndsAt = day(2026, 11, 1)
)

// Step is one change in the story. Apply is nil for a step that happens
// by itself as time passes.
type Step struct {
	At    time.Time
	Name  string
	Apply func(*Org) error
}

// Story returns the fictional org's timeline in order. A test plays it by
// setting the clock to each step's At and calling its Apply.
func Story() []Step {
	return []Step{
		{At: RepoRenamedAt, Name: "rename payments-api to payments", Apply: func(o *Org) error {
			return o.RenameRepo("payments-api", "payments")
		}},
		{At: CodeownersChangedAt, Name: "payments CODEOWNERS: @acme/payments to @acme/platform", Apply: func(o *Org) error {
			return o.SetFile("payments", ".github/CODEOWNERS", "# Payments service\n* @acme/platform\n")
		}},
		{At: TeamRenamedAt, Name: "rename team sre to Reliability", Apply: func(o *Org) error {
			return o.RenameTeam("sre", "Reliability")
		}},
		{At: TeamDeletedAt, Name: "delete team legacy-ops", Apply: func(o *Org) error {
			return o.DeleteTeam("legacy-ops")
		}},
		{At: MembershipEndsAt, Name: "lfischer's engineering membership ends in the directory"},
	}
}

func seedPeople() []*Person {
	return []*Person{
		{
			ID: "jdoe", Name: "Jane Doe", Email: "jdoe@acme.example",
			GitHub:    &GitHubUser{DatabaseID: 56030835, Login: "jdoe", VerifiedEmails: []string{"jdoe@acme.example"}},
			Directory: &DirectoryUser{PK: 42, UUID: "7f3c2a9e-1b4d-4c8e-9a0f-2d6e8b1c5a74", Username: "jdoe", GitHubConnection: true, LinkLogin: true},
		},
		{
			ID: "rpatel", Name: "Raj Patel", Email: "rpatel@acme.example",
			GitHub:    &GitHubUser{DatabaseID: 56030901, Login: "rpatel", VerifiedEmails: []string{"rpatel@acme.example"}},
			Directory: &DirectoryUser{PK: 43, UUID: "0d1e2f3a-4b5c-4d6e-8f70-8192a3b4c5d6", Username: "rpatel", GitHubConnection: true, LinkLogin: true},
		},
		{
			// Different usernames in the two systems: only the link joins them.
			ID: "mchen", Name: "Mei Chen", Email: "mchen@acme.example",
			GitHub:    &GitHubUser{DatabaseID: 41022233, Login: "meichen", VerifiedEmails: []string{"mchen@acme.example"}},
			Directory: &DirectoryUser{PK: 44, UUID: "3a4b5c6d-7e8f-4a0b-9c1d-2e3f4a5b6c7d", Username: "mchen", GitHubConnection: true, LinkLogin: true},
		},
		{
			// The directory records only the GitHub login: not authoritative.
			ID: "tbecker", Name: "Tom Becker", Email: "tbecker@acme.example",
			GitHub:    &GitHubUser{DatabaseID: 61509377, Login: "tbecker", VerifiedEmails: []string{"tbecker@acme.example"}},
			Directory: &DirectoryUser{PK: 45, UUID: "6c7d8e9f-0a1b-4c2d-8e3f-4a5b6c7d8e9f", Username: "tbecker", LinkLogin: true},
		},
		{
			// A contractor: GitHub only, no verified-domain email.
			ID: "sokafor", Name: "Sam Okafor", Email: "sam@okafor.example",
			GitHub: &GitHubUser{DatabaseID: 73440012, Login: "sokafor-ext"},
		},
		{
			// A manager with no GitHub account.
			ID: "lfischer", Name: "Lena Fischer", Email: "lfischer@acme.example",
			Directory: &DirectoryUser{PK: 46, UUID: "9e8d7c6b-5a49-4382-b716-05f4e3d2c1b0", Username: "lfischer"},
		},
	}
}

func seedTeams() []*Team {
	return []*Team{
		{
			ID: "engineering", Description: "All of engineering",
			GitHub:    &GitHubTeam{DatabaseID: 1920001, Name: "Engineering", Slug: "engineering"},
			Directory: &DirectoryGroup{UUID: "2a7d9c10-4e3b-4f6a-9b21-0c5d8e7f6a13", NumPK: 1001, Name: "engineering"},
		},
		{
			ID: "payments", Description: "Payment processing", Parent: "engineering",
			GitHub:    &GitHubTeam{DatabaseID: 1920002, Name: "Payments", Slug: "payments"},
			Directory: &DirectoryGroup{UUID: "5b0e2c1d-3f4a-4b5c-8d6e-7f8091a2b3c4", NumPK: 1002, Name: "payments"},
		},
		{
			ID: "platform", Description: "Shared platform and tooling", Parent: "engineering",
			GitHub:    &GitHubTeam{DatabaseID: 1920003, Name: "Platform", Slug: "platform"},
			Directory: &DirectoryGroup{UUID: "9c4e1f2a-6b7d-4e8f-a019-3b2c4d5e6f70", NumPK: 1003, Name: "platform"},
		},
		{
			ID: "sre", Description: "Site reliability", Parent: "engineering",
			GitHub: &GitHubTeam{DatabaseID: 1920004, Name: "SRE", Slug: "sre"},
		},
		{
			ID: "legacy-ops", Description: "Operations, before the platform team",
			GitHub: &GitHubTeam{DatabaseID: 1810077, Name: "Legacy Ops", Slug: "legacy-ops"},
		},
	}
}

func seedRepos() []*Repo {
	return []*Repo{
		{
			DatabaseID: 412009871, Name: "ops-scripts", Description: "Retired operations scripts",
			Language: "Shell", Archived: true, DefaultBranch: "main", Visibility: "internal",
			CreatedAt: day(2021, 3, 4), UpdatedAt: day(2024, 6, 30), PushedAt: day(2024, 6, 30),
			// .github/CODEOWNERS wins; the root file is stale and ignored.
			Files: map[string]string{
				".github/CODEOWNERS": "* @acme/legacy-ops\n",
				"CODEOWNERS":         "* @acme/platform\n",
			},
		},
		{
			DatabaseID: 525776495, Name: "payments-api", Description: "Payment processing API",
			Language: "Go", Topics: []string{"payments", "tier-1"}, DefaultBranch: "main", Visibility: "private",
			CreatedAt: day(2023, 1, 9), UpdatedAt: day(2026, 9, 20), PushedAt: day(2026, 9, 20),
			Files: map[string]string{".github/CODEOWNERS": "# Payments service\n* @acme/payments\n"},
		},
		{
			// The root CODEOWNERS wins; docs/CODEOWNERS is stale and ignored.
			DatabaseID: 525776502, Name: "web", Description: "Customer web app",
			Language: "TypeScript", Topics: []string{"frontend"}, DefaultBranch: "main", Visibility: "private",
			CreatedAt: day(2023, 1, 10), UpdatedAt: day(2026, 9, 25), PushedAt: day(2026, 9, 25),
			Files: map[string]string{
				"CODEOWNERS":      "* @acme/platform\n/docs/ @jdoe\n",
				"docs/CODEOWNERS": "* @acme/payments\n",
			},
		},
		{
			// Only path rules, one naming the team that is renamed later.
			DatabaseID: 525776510, Name: "handbook", Description: "Engineering handbook",
			DefaultBranch: "main", Visibility: "public",
			CreatedAt: day(2023, 2, 1), UpdatedAt: day(2026, 8, 14), PushedAt: day(2026, 8, 14),
			Files: map[string]string{"docs/CODEOWNERS": "/guides/ @acme/platform\n/runbooks/ @acme/sre\n"},
		},
		{
			DatabaseID: 601233458, Name: "sandbox", DefaultBranch: "trunk", Visibility: "public",
			CreatedAt: day(2025, 7, 1), UpdatedAt: day(2025, 7, 1), PushedAt: day(2025, 7, 1),
			Files: map[string]string{"README.md": "# sandbox\n"},
		},
	}
}

func seedMemberships() []*Membership {
	return []*Membership{
		{Person: "jdoe", Team: "payments", Role: "maintainer", From: day(2026, 3, 1)},
		{Person: "rpatel", Team: "payments", Role: "member", From: day(2025, 6, 1)},
		{Person: "mchen", Team: "platform", Role: "maintainer", From: day(2024, 2, 12)},
		{Person: "tbecker", Team: "platform", Role: "member", From: day(2026, 1, 5)},
		{Person: "tbecker", Team: "sre", Role: "member", From: day(2025, 11, 1)},
		{Person: "sokafor", Team: "sre", Role: "member", From: day(2026, 8, 15)},
		{Person: "sokafor", Team: "legacy-ops", Role: "maintainer", From: day(2023, 4, 1)},
		// Directory only: lfischer has no GitHub account, and GitHub has no
		// scheduled ends. The directory records the end in advance.
		{Person: "lfischer", Team: "engineering", Role: "maintainer", From: day(2024, 1, 1), Until: MembershipEndsAt},
	}
}
