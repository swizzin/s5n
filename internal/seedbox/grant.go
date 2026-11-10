package seedbox

import "strings"

// Rule is one grant-owned key, written as a chart values path.
type Rule struct {
	// Path is a dotted values path. "*" matches any single key, which is how
	// "every app" is written without listing the catalog here.
	Path string
	// Whole says the value at Path is owned as a unit: everything beneath it
	// belongs to the grant too, and values may not touch any of it. It is set
	// for lists and for objects whose every field is a decision, never for a
	// branch that merely contains grant keys.
	Whole bool
	// BoxOnly marks a key that is box's own record rather than a chart value.
	// namespace is one: it names the release namespace, so box reads it and
	// never hands it to Helm. storage.<kind>.path is the other: it is the host
	// directory box turns into a PersistentVolume, so it is read, never charted.
	BoxOnly bool
	// BoxSets marks a chart key box writes from its own decision, which no file
	// may set. The claim names are one: box derives them from the namespace and
	// creates the claim, so a name in either file would either be overwritten
	// or clone the volume. auth.edge is the other: box puts a login in front of
	// every release, and the chart ships none.
	BoxSets bool
	// Fixed marks a BoxSets key box writes as a constant rather than deriving
	// it from the grant. auth.edge is the only one: it says a login is in
	// front, which is a fact about box's deployment, not about this grant.
	Fixed bool
	// Why is the line box shows when a user's values file sets this key. It
	// should say what the grant is protecting, not repeat the key name.
	Why string
}

// GrantKeys is the grant's key set. The principle behind it: a key belongs to
// the grant when it decides what this box can reach or which apps exist — host
// names, the namespace, paths on the host, uids, ports exposed on the node, the
// storage roots, the network boundary, and the app and instance list. Every
// other key is the user's: settings inside an app they were given, tuning,
// image variants, schedules.
//
// The judgement calls, and why they land where they do:
//
//   - auth.edge is BoxSets, and Fixed: the chart ships no login and refuses to
//     render without the statement that one is in front, so box writes true
//     when it builds the release's values (Effective), for every release whose
//     schema declares it. It is a fact about the deployment, not a setting a
//     file gets to deny.
//   - global.tz is values. It changes how a user's own apps name files, and
//     nothing about who can read them.
//   - storage.<kind>.path is grant and box-only: it is where this box's volume
//     lives on the host, which is the same class of decision as the domain. The
//     chart's storage.<kind>.existingClaim is set by box, from the claim it
//     creates for that path, so it is BoxSets: neither file names a claim, and
//     neither file has a size or a storage class to set any more.
//   - storage.*.size is gone with the chart's storage objects. A static claim's
//     capacity is nominal and a per-user cap would be a quota, which PLAN.md
//     keeps out.
//   - gateway.enabled is values: taking your own box off the air is yours.
//     gateway.parentRef.* is grant: the Gateway and the listener belong to
//     root, and a user who could point at another listener could attach their
//     routes to somebody else's host name.
//   - networkPolicy.enabled is grant. It is the default-deny between boxes, so
//     a user must not be able to switch it off; widening ingress to their own
//     apps (gatewayFrom, extraHttpFrom) stays theirs.
//   - apps.*.instances is grant as a whole list: a suffix becomes a host name
//     (sonarr-4k), a category becomes a downloads directory, and enabled
//     decides existence. There is nothing left in an instance for a user to
//     tune, and a user who could add an instance could add a second category
//     under their own name only — but also a second host name, which root
//     routes. So the list is one grant decision, never merged entry by entry.
//   - apps.qbittorrent.torrent.port and .service.type are grant because a
//     NodePort is a port on the node, shared with every other box. docs/box.md
//     leaves port assignment open; until root assigns it, the grant is where it
//     would live, so it is grant now rather than a value that has to move
//     later. The Service's annotations are grant for the same reason: they
//     choose the address a LoadBalancer gets.
//   - apps.*.image is values, but only as a choice: the variant name and a
//     tag or digest pin inside that variant's repository. The repository
//     comes from the chart's own variant list, and values.schema.json rejects
//     a repository key outright, so this is not a grant key — it is not a key
//     a user has at all.
var GrantKeys = []Rule{
	{Path: "namespace", BoxOnly: true, Why: "the release namespace: one user is one namespace"},

	// The chart ships no login; box is what puts one in front of every release,
	// so no file may say otherwise. Fixed: box writes true rather than deriving
	// it from anything in the grant.
	{Path: "auth.edge", BoxSets: true, Fixed: true, Why: "box puts a login in front of every release, and the chart refuses to render one without it"},

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
	// gatewayFrom is box's because it is read from the cluster: the namespace
	// the chosen Gateway implementation's proxy actually runs in. A user who
	// could set it could widen their apps to whatever namespace they named,
	// and it would be overwritten on the next apply anyway.
	{Path: "networkPolicy.gatewayFrom", Whole: true, BoxSets: true, Why: "box reads where the Gateway's proxy runs from the Gateway's own address, so the peer follows the implementation in use rather than a namespace a file guessed"},

	{Path: "apps.*.enabled", Why: "which apps this box has"},
	{Path: "apps.*.instances", Whole: true, Why: "which instances exist: a suffix is a host name and a category is a directory"},

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
