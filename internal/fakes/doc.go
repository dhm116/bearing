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
//   - jdoe, Jane Doe: GitHub jdoe and directory jdoe, linked by the
//     directory's GitHub OAuth connection and her recorded login.
//   - rpatel, Raj Patel: the same in both, linked the same way.
//   - mchen, Mei Chen: GitHub meichen, directory mchen, linked the same
//     way: the names differ, so only the links join them.
//   - tbecker, Tom Becker: in both, but the directory records only his
//     GitHub login, which is not authoritative evidence.
//   - sokafor, Sam Okafor: a contractor, GitHub only (sokafor-ext).
//   - lfischer, Lena Fischer: a manager, directory only.
//
// Teams: engineering (parent of payments, platform and sre), payments and
// platform exist in both systems; sre and legacy-ops only in GitHub.
// Repositories (visibility in brackets):
//
//   - ops-scripts [internal]: archived; .github/CODEOWNERS
//     "* @acme/legacy-ops" and a stale root CODEOWNERS that GitHub ignores.
//   - payments-api [private]: .github/CODEOWNERS "* @acme/payments".
//   - web [private]: root CODEOWNERS with a "*" rule and a path rule for a
//     person; a stale docs/CODEOWNERS that GitHub ignores.
//   - handbook [public]: docs/CODEOWNERS, only path rules, one naming
//     @acme/sre.
//   - sandbox [public]: no CODEOWNERS, default branch "trunk".
//
// Memberships start at their From time. GitHub lists a membership until
// [Org.EndMembership] removes it, which also sends a membership.removed
// delivery. The directory also honours an end date it records in advance
// (Until): lfischer is in engineering until [MembershipEndsAt]
// (2026-11-01), and the directory stops listing her then with no change
// to the org. GitHub never sees that membership: she has no account.
//
// # Timeline
//
// [Story] lists the changes, at [RepoRenamedAt], [CodeownersChangedAt],
// [TeamRenamedAt] and [TeamDeletedAt]: payments-api is renamed to payments
// (same node ID, old name redirects); its CODEOWNERS moves from
// @acme/payments to @acme/platform; sre is renamed to Reliability (slug
// reliability, the old slug stops resolving, handbook's CODEOWNERS still
// names it); legacy-ops is deleted (ops-scripts' CODEOWNERS still names
// it). lfischer's engineering membership ends at [MembershipEndsAt] with no
// change at all. The recorded directory feed,
// testdata/acme/directory.ndjson at the repository root, was read at
// [DirectorySyncAt].
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
// X-Github-Next-Global-ID: 1, legacy otherwise. Credentials are
// "Authorization: Bearer <token>" or "token <token>"; a token other than
// [GitHubOptions.Token] gets a 401. Without an Authorization header, REST
// serves only public repositories (other repositories and all teams are
// 404) and GraphQL answers 401. Every response, errors included, carries
// the X-RateLimit-* headers, and [GitHub.RateLimitNext] makes the next
// requests fail with GitHub's 403 rate-limit reply.
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
// Their node_ids are legacy-format, because no header selects the format
// for webhooks: an adapter derives the next-format ID from the numeric
// "id" (as [GitHubUser.NodeID] does) and never emits a legacy ID. Push
// payloads give the repository's created_at and pushed_at as Unix seconds,
// as GitHub does.
//
// # Directory endpoints
//
// A subset of Authentik's API, paged by page and page_size with
// Authentik's pagination object. With [DirectoryOptions.Token] set,
// requests need "Authorization: Bearer <token>"; others get Authentik's
// 403 with a "detail".
//
//   - GET /api/v3/core/users/ and /api/v3/core/users/{pk}/: users with
//     their uuid, username, name, email, groups and attributes. The
//     attribute "github" holds a "login" an admin recorded.
//   - GET /api/v3/core/groups/ and /api/v3/core/groups/{uuid}/: groups with
//     parent, users (primary keys) and, in attributes,
//     "membership_periods": the recorded start and end of each current
//     member's membership.
//   - GET /api/v3/sources/user_connections/oauth/: users' connections to
//     the GitHub OAuth source (filters user and source__slug); identifier
//     is the numeric GitHub user ID.
//
// A directory adapter reads the OAuth connections for authoritative
// links, deriving the next-format GitHub node ID from the identifier, and
// attributes.github.login for the non-authoritative login links.
package fakes
