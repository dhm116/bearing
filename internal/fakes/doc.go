// Package fakes holds httptest fakes of the systems Bearing's adapters
// read: a GitHub API ([NewGitHub], REST and GraphQL), an Authentik-like
// identity directory ([NewDirectory]) and a Kubernetes API
// ([NewKubernetes]). The first two serve one fictional organization, an
// [Org], so the two systems can't drift apart; the third serves the
// company's cluster, a [Cluster]. Tests, golden tests and demos use them;
// production code never imports them.
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
// the X-RateLimit-* headers. Credentials are checked before the rate limit,
// so a bad token gets a 401 and is not counted. Authenticated callers have a
// budget of 5000 requests and anonymous callers their own of 60; a request
// over budget, or one of the next n after [GitHub.RateLimitNext](n), gets
// GitHub's 403 rate-limit reply.
//
//   - GET /orgs/acme/repos, /orgs/acme/teams and
//     /orgs/acme/teams/{slug}/members, paged by page and per_page with Link
//     headers. Team members include child teams' members, as on GitHub.
//   - GET /orgs/acme/teams/{slug}, GET /repos/acme/{repo} and
//     GET /repos/acme/{repo}/contents/{path}: raw text with a ".raw" media
//     type, else a contents object. Old repository names answer 301 to
//     /repositories/{id}/..., which is also served.
//   - POST /graphql: the operations in [GraphQLQueries], with cursor
//     paging in database ID order: Repositories, Teams (with each team's
//     direct child teams) and TeamMembers list the org; Repository and
//     Team read one object by node ID (either format), as an adapter
//     re-reads after a webhook, with a null node once it is gone.
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
//
// # The fictional cluster
//
// Acme's k3d cluster, [ClusterID] "k3d-acme", has the namespaces payments,
// billing and kube-system (the only one the adapter's default
// exclude_namespaces covers here; default, kube-public and kube-node-lease
// are left out). Its workloads:
//
//   - payments-api, a Deployment in payments: the spec's example input
//     (docs/spec/adapter-protocol.md). Labels team=payments and tier=gold,
//     annotations github.com/project-slug=acme/payments-api and the
//     Deployment controller's revision, one container with a fake token
//     argument and a fake password variable. It starts at image 1.4.1 on 2
//     replicas and is what sample 9 reports once the story has run to
//     [ImageBumpedAt]. Its team label stays payments while its repository's
//     CODEOWNERS moves to @acme/platform at [CodeownersChangedAt], so the
//     two disagree from then on.
//   - legacy-billing, a Deployment in billing: no team label, no
//     annotation, an image from a registry the adapter doesn't map.
//   - billing-core, a StatefulSet in billing: label team=billing, no
//     annotation, a ghcr.io image (the image hint), an init container, and
//     the canary: a fake password in an environment variable, in a
//     command-line argument and in the name of the Secret its envFrom
//     reads. [Canaries] lists every such value, payments-api's included.
//   - svclb-traefik (a DaemonSet) and coredns (a Deployment) in
//     kube-system, which the adapter does not read.
//
// [KubernetesStory] lists the changes, at [ReplicasScaledAt] (payments-api
// goes from 2 replicas to 3), [ImageBumpedAt] (1.4.1 to 1.4.2, the Tuesday
// the spec's samples use), [TeamLabelClearedAt] (billing-core's team label
// becomes ""), [AnnotationEditedAt] (payments-api's annotation becomes
// TODO), [WorkloadDeletedAt] (legacy-billing) and [NamespaceExcludedAt]
// (billing joins the adapter's exclude_namespaces: nothing changes in the
// cluster, so [ExcludedNamespaces] gives the setting at a time). Mutations
// ([Cluster.SetImage], [Cluster.SetReplicas], [Cluster.SetLabel],
// [Cluster.SetAnnotation], [Cluster.DeleteWorkload], [Cluster.AddWorkload],
// [Cluster.AddNamespace]) take their time from the Cluster's clock and
// stamp it in the object's metadata.managedFields. The lists as the
// adapter reads them at [ImageBumpedAt] are recorded in
// testdata/acme/kubernetes.ndjson at the repository root.
//
// # Kubernetes endpoints
//
// Bearer tokens only: a request with none, or with a token other than
// [KubernetesOptions.Token], gets the API server's 401 Status. The token
// is the adapter's read-only one ([ServiceAccountUser]): it may list
// namespaces and Deployments, StatefulSets and DaemonSets, and every other
// call is a 403 Status naming what was refused, whether a Secret, a
// ConfigMap, a Pod, a get of one workload, a watch or a write. The token
// is checked first, so an unknown caller gets the 401 even on a refused
// path. All of it, the refusals included, is in the request log. Paths
// outside /api and /apis are refused too, /version and discovery included,
// though a real cluster lets any authenticated caller read them: an adapter
// that probes them would fail here and pass there.
//
//   - GET /api/v1/namespaces
//   - GET /apis/apps/v1/deployments, /statefulsets and /daemonsets, and the
//     same under /apis/apps/v1/namespaces/{namespace}/. A namespace that
//     doesn't exist lists as empty.
//
// Lists are Kubernetes Lists (items without kind or apiVersion, a
// resourceVersion) in key order, paged by limit and continue: the token is
// opaque, names the last item served so a change between pages moves
// nothing already read, and belongs to one list (another list's token is a
// 400). With [KubernetesOptions.ExpireContinue] or [Kubernetes.ExpireContinue]
// a valid continue token gets 410 Gone (a malformed one, or one from
// another list, is a 400 first). Objects carry more than the adapter may
// read (status, managedFields, the pod template's environment and
// arguments), as a real cluster's do. A container's environment is plain
// name and value pairs; valueFrom, configMapRef and ports are not modeled.
package fakes
