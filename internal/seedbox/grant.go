package seedbox

import "strings"

// Rule is one grant-owned key, written as a chart values path.
type Rule struct {
	// Path is a dotted values path; "*" matches any single key, which is how
	// "every app" is written without listing the catalog here.
	Path string
	// Whole says the value at Path is owned as a unit: everything beneath it
	// belongs to the grant too. It is set for lists and for objects whose every
	// field is a decision, never for a branch that merely contains grant keys.
	Whole bool
	// BoxOnly marks a key that is box's own record rather than a chart value:
	// namespace, storage.<kind>.path and auth.oidc.gatewayIP are read by box and
	// never handed to Helm.
	BoxOnly bool
	// BoxSets marks a chart key box writes from its own decision, which no file
	// may set: the claim names and auth.edge.
	BoxSets bool
	// Fixed marks a BoxSets key box writes as a constant rather than deriving it
	// from the grant. auth.edge is the only one.
	Fixed bool
	// Why is the line box shows when a user's values file sets this key; it says
	// what the grant is protecting, not the key's name.
	Why string
}

// GrantKeys is the grant's key set: a key belongs to the grant when it decides
// what this box can reach or which apps exist -- host names, the namespace, host
// paths, uids, node ports, storage roots, the network boundary, and the app and
// instance list. Everything else is the user's: tuning, image variants,
// schedules.
//
// The judgement calls:
//   - auth.edge is BoxSets and Fixed: the chart ships no login and refuses to
//     render without the statement that one is in front, so box writes true in
//     Effective, for every release whose schema declares it.
//   - storage.<kind>.path is grant and BoxOnly; the claim name is box's, from
//     that path, so neither file names a claim or a size.
//   - gateway.enabled is values (taking your own box off the air is yours);
//     gateway.parentRef.* is grant, because a user pointing at another listener
//     could attach their routes to somebody else's host name.
//   - networkPolicy.enabled is grant: it is the default-deny between boxes.
//     Widening ingress to their own apps stays theirs.
//   - apps.*.instances is grant as a whole list: a suffix becomes a host name and
//     a category a downloads directory, so the list is one decision, never merged
//     entry by entry.
//   - apps.qbittorrent.torrent.port and .service.type are grant: a NodePort is a
//     port on the node, shared with every other box.
//   - apps.*.image is values only as a choice: the variant name and a pin inside
//     its repository. The repository is the chart's, and the schema rejects one.
var GrantKeys = []Rule{
	{Path: "namespace", BoxOnly: true, Why: "the release namespace: one user is one namespace"},

	// login is the account root's login admits: box's record of who the box
	// belongs to, never a chart value. Still handed to the release, because an
	// app that keeps its own accounts (Jellyfin) names the person's first account
	// after it.
	{Path: "login", BoxOnly: true, Why: "the account root's login creates: this box's own identity, and the name one app's own first account gets"},

	// The chart ships no login; box puts one in front of every release. Fixed: true
	// is written, not derived.
	{Path: "auth.edge", BoxSets: true, Fixed: true, Why: "box puts a login in front of every release, and the chart refuses to render one without it"},

	// The Gateway proxy's address, so an OIDC app's pod reaches the issuer at the
	// box's own name. Box's record: a user cannot read the Gateway's namespace, and
	// an app that resolved the name any other way would fail the certificate check.
	{Path: "auth.oidc.gatewayIP", BoxOnly: true, Why: "the address of the Gateway's proxy, which is how an OIDC app's pod reaches the login at the box's own name"},

	{Path: "global.puid", Why: "the uid every file on the data volume is owned by"},
	{Path: "global.pgid", Why: "the gid every file on the data volume is owned by"},

	{Path: "storage.data.path", BoxOnly: true, Why: "where this box's downloads and media live on the host"},
	{Path: "storage.config.path", BoxOnly: true, Why: "where this box's app configs live on the host"},
	// The chart's storage keys, as a unit: box names the claims it creates and
	// there is nothing under storage for a file to decide.
	{Path: "storage", Whole: true, BoxSets: true, Why: "box creates the claims from the grant's host paths, and a static claim has no size or storage class to set"},

	{Path: "gateway.host", Why: "the box's domain, which is how it is reached"},
	{Path: "gateway.parentRef.name", Why: "the Gateway that serves this box"},
	{Path: "gateway.parentRef.namespace", Why: "the Gateway that serves this box"},
	{Path: "gateway.parentRef.sectionName", Why: "the listener root opened for this box"},

	{Path: "networkPolicy.enabled", Why: "the default-deny that keeps one box off another"},
	// Box's own record of what kind of box this is: written when it applies a
	// rehearsal snapshot and read on every later apply and update, so the lock is
	// sticky. A box with it set refuses a final snapshot.
	{Path: "rehearsal", BoxOnly: true, Why: "a rehearsal box has no internet egress; box sets this when it applies a rehearsal snapshot, and nothing unlocks it"},
	{Path: "rehearsalGate", BoxOnly: true, Why: "the address a rehearsal box's apps wait on until the lock closes it; box writes it with rehearsal"},

	// Read from the cluster: the namespace the chosen Gateway implementation's
	// proxy actually runs in. A user who could set it could widen their apps to
	// whatever namespace they named.
	{Path: "networkPolicy.gatewayFrom", Whole: true, BoxSets: true, Why: "box reads where the Gateway's proxy runs from the Gateway's own address, so the peer follows the implementation in use rather than a namespace a file guessed"},

	{Path: "apps.*.enabled", Why: "which apps this box has"},
	{Path: "apps.*.instances", Whole: true, Why: "which instances exist: a suffix is a host name and a category is a directory"},

	// The cluster's pod network, read from the Nodes at init: only root can read
	// the cluster, and a user naming a range they do not own would make the app
	// trust any client in it. BoxOnly, not BoxSets, because init is the write.
	{Path: "apps.jellyfin.knownProxies", Whole: true, BoxOnly: true, Why: "the cluster's pod network, read from the Nodes at init: it is the range the Gateway's proxy connects from, and an app cannot tell whose forwarded address to trust without it"},

	{Path: "apps.qbittorrent.torrent.port", Why: "the peer port exposed on the node, shared with every other box"},
	{Path: "apps.qbittorrent.torrent.service.type", Why: "whether the peer port is on the node at all"},
	{Path: "apps.qbittorrent.torrent.service.annotations", Whole: true, Why: "the address and sharing of the peer Service"},
}

// GrantRule reports which rule owns a key path, if any.
func GrantRule(keys []string) (Rule, bool) {
	for _, r := range GrantKeys {
		if r.Owns(keys) {
			return r, true
		}
	}
	return Rule{}, false
}

// Owns matches a key path against the rule. A whole rule owns its path and
// everything below it; a leaf rule owns exactly its path.
func (r Rule) Owns(keys []string) bool {
	want := strings.Split(r.Path, ".")
	if r.Whole {
		if len(keys) < len(want) {
			return false
		}
		keys = keys[:len(want)]
	}
	if len(keys) != len(want) {
		return false
	}
	for i := range want {
		if want[i] != "*" && want[i] != keys[i] {
			return false
		}
	}
	return true
}
