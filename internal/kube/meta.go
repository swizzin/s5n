package kube

// The strings box stamps on everything it owns, and the field managers it
// writes them under.
//
// They are how box recognises its own work on a cluster it did not create: the
// GatewayClass and Gateway it applied, the Helm releases it installed, the
// admission policy it enforces, the seedbox namespace root writes at `box
// adduser`. A copy that keeps an old value while the others move does not fail
// loudly -- it makes box stop recognising an object it installed, and then
// refuse to touch it or install beside it.
const (
	// ManagedByLabel and ManagedBy are the app.kubernetes.io/managed-by=box
	// pair. The label says who created the object; Helm v4 stores release
	// labels beside the release record, so on a release it survives upgrades
	// and is readable without parsing chart values.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedBy      = "box"

	// PartOfLabel and PartOf say what an object belongs to: the s5n platform
	// or a seedbox, not one user's release. It is what makes box's objects
	// findable as a set.
	PartOfLabel = "app.kubernetes.io/part-of"
	PartOf      = "s5n"

	// FieldManager is box's server-side-apply field manager. It is what makes
	// a second run field-for-field the same write as the first, and it is the
	// manager `kubectl apply --field-manager=box` agrees with.
	FieldManager = "box"

	// TLSManagerPrefix is the field-manager prefix the per-host TLS flow
	// writes one host's Gateway listeners under, besides FieldManager. A
	// Gateway's listeners are a map keyed by name, so a partial apply of one
	// host's listeners has to be owned by a manager of its own: the platform's
	// full Gateway apply must not be able to remove them, and one host's
	// manager must not be able to remove another host's.
	TLSManagerPrefix = "box-tls-"
)
