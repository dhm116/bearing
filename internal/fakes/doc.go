// Package fakes holds httptest fakes of the systems Bearing's adapters
// read: a GitHub API ([NewGitHub], REST and GraphQL) and an Authentik-like
// identity directory ([NewDirectory]). Both serve one fictional
// organization, an [Org], so the two systems can't drift apart. Tests,
// golden tests and demos use it; production code never imports it.
//
// # The fictional org
//
// Acme Corp (GitHub org "acme", domain acme.example). People, by seed ID:
//
//   - jdoe, Jane Doe: GitHub jdoe and directory jdoe; the directory links
//     her GitHub node ID and login.
//   - rpatel, Raj Patel: the same in both, linked by node ID and login.
//   - mchen, Mei Chen: GitHub meichen, directory mchen, linked by node ID:
//     the names differ, so only the link joins them.
//   - tbecker, Tom Becker: in both, but the directory records only his
//     GitHub login, which is not authoritative evidence.
//   - sokafor, Sam Okafor: a contractor, GitHub only (sokafor-ext).
//   - lfischer, Lena Fischer: a manager, directory only.
//
// Teams: engineering (parent of payments, platform and sre), payments and
// platform exist in both systems; sre and legacy-ops only in GitHub.
// Repositories: ops-scripts (archived, CODEOWNERS "* @acme/legacy-ops"),
// payments-api (".github/CODEOWNERS" "* @acme/payments"), web (root
// CODEOWNERS with a "*" rule and a path rule for a person; a stale
// docs/CODEOWNERS that GitHub ignores), handbook (only path rules, one
// naming @acme/sre) and sandbox (no CODEOWNERS, branch "trunk").
//
// Memberships are time-bounded on [From, Until) and both systems list them
// while active. jdoe is in payments from 2026-03-01 until
// [MembershipEndsAt] (2026-11-01); the rest are open-ended.
//
// # Timeline
//
// [Story] lists the changes, at [RepoRenamedAt], [CodeownersChangedAt],
// [TeamRenamedAt] and [TeamDeletedAt]: payments-api is renamed to payments
// (same node ID, old name redirects); its CODEOWNERS moves from
// @acme/payments to @acme/platform; sre is renamed to Reliability (slug
// reliability, the old slug stops resolving, handbook's CODEOWNERS still
// names it); legacy-ops is deleted (ops-scripts' CODEOWNERS still names
// it). jdoe's membership ends at [MembershipEndsAt] with no change at all.
// The recorded directory feed, testdata/acme/directory.ndjson at the
// repository root, was read at [DirectorySyncAt].
//
// Mutations ([Org.RenameRepo], [Org.SetFile], [Org.DeleteFile],
// [Org.RenameTeam], [Org.DeleteTeam], [Org.AddMembership],
// [Org.EndMembership]) take their time from the Org's clock, so a test
// sets a testkit.FakeClock and then mutates. Membership activity is
// evaluated at the clock's time on every request.
//
// # GitHub endpoints
//
// Node IDs are next-format when the request sends
// X-Github-Next-Global-ID: 1, legacy otherwise. With
// [GitHubOptions.Token] set, requests need "Authorization: Bearer <token>".
//
//   - GET /orgs/acme/repos, /orgs/acme/teams and
//     /orgs/acme/teams/{slug}/members, paged by page and per_page with Link
//     headers. Team members include child teams' members, as on GitHub.
//   - GET /orgs/acme/teams/{slug}, GET /repos/acme/{repo} and
//     GET /repos/acme/{repo}/contents/{path}: raw text with a ".raw" media
//     type, else a contents object. Old repository names answer 301 to
//     /repositories/{id}/..., which is also served.
//   - POST /graphql: the operations in [GraphQLQueries] (Repositories,
//     Teams, TeamMembers), with cursor paging in database ID order.
//
// [GitHub.Deliveries] renders the org's changes as webhook deliveries
// (repository renamed, push, team edited or deleted, membership added or
// removed), signed with [GitHubOptions.WebhookSecret] as [Sign] does.
//
// # Directory endpoints
//
// A subset of Authentik's core API, paged by page and page_size with
// Authentik's pagination object, and with [DirectoryOptions.Token] as a
// bearer token:
//
//   - GET /api/v3/core/users/ and /api/v3/core/users/{pk}/: users with
//     their uuid, username, name, email, groups and attributes. The
//     attribute "github" holds the linked "node_id" and "login".
//   - GET /api/v3/core/groups/ and /api/v3/core/groups/{uuid}/: groups with
//     parent, users (primary keys) and, in attributes,
//     "membership_periods": the start and end of each current member's
//     membership.
package fakes
