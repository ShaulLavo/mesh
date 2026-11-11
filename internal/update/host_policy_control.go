package update

type HostPolicyChange struct {
	ExpectedRevision uint64     `json:"expectedRevision"`
	Policy           HostPolicy `json:"policy"`
}
