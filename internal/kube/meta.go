package kube

// The strings box stamps on everything it owns, and the field managers it writes
// them under. They are how box recognises its own work on a cluster it did not
// create: a copy that keeps an old value while the others move makes box stop
// recognising an object it installed, and then refuse to touch it or install
// beside it.
const (
	// ManagedByLabel and ManagedBy are the app.kubernetes.io/managed-by=box pair,
	// saying who created the object. Helm v4 stores release labels beside the
	// release record, so on a release it survives upgrades without parsing values.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedBy      = "box"

	// PartOfLabel and PartOf say what an object belongs to: the s5n platform or a
	// seedbox, not one user's release, which makes box's objects a findable set.
	PartOfLabel = "app.kubernetes.io/part-of"
	PartOf      = "s5n"

	// FieldManager is box's server-side-apply field manager: it makes a second run
	// field-for-field the same write as the first, and `kubectl apply
	// --field-manager=box` agrees with it.
	FieldManager = "box"

	// TLSManagerPrefix names the per-host TLS flow's field managers, beside
	// FieldManager. A Gateway's listeners are one map keyed by name and an apply
	// removes what its manager leaves out, so each host's listeners need a manager
	// of their own: the platform's full Gateway apply must not remove them, nor
	// one host's manager another host's. The flow uses one manager per listener,
	// hence the suffix.
	TLSManagerPrefix = "box-tls-"
)
