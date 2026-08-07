package nostrscan

// Role identifies the Nostr-facing responsibility of a repository.
type Role string

const (
	RoleClient  Role = "client"
	RoleRelay   Role = "relay"
	RoleSigner  Role = "signer"
	RoleLibrary Role = "library"
	RoleDVM     Role = "dvm"
)

// MarkerKind identifies a class of deterministic detection evidence.
type MarkerKind string

const (
	MarkerDependency MarkerKind = "dependency"
	MarkerProtocol   MarkerKind = "protocol"
	MarkerStructural MarkerKind = "structural"
	MarkerRole       MarkerKind = "role"
)

// Marker is one auditable reason for a detector result.
type Marker struct {
	Kind   MarkerKind `json:"kind"`
	Name   string     `json:"name"`
	Path   string     `json:"path"`
	Line   int        `json:"line,omitempty"`
	Weight float64    `json:"weight,omitempty"`
	Detail string     `json:"detail"`
}

// NostrProfile is the deterministic classification of one repository checkout.
type NostrProfile struct {
	IsNostr    bool     `json:"is_nostr"`
	Confidence float64  `json:"confidence"`
	Roles      []Role   `json:"roles"`
	Evidence   []Marker `json:"evidence"`
}
