package protocol

const (
	TypeFleetHosts       = "fleet.hosts"
	TypeFleetHostsResult = "fleet.hosts.result"
)

// FleetHost carries a pinned connection. Names come from the destination's
// authenticated host declaration, never from another controller's address book.
type FleetHost struct {
	ID            string   `json:"id"`
	MeshIdentity  string   `json:"meshIdentity"`
	TailscaleName string   `json:"tailscaleName,omitempty"`
	Addresses     []string `json:"addresses,omitempty"`
	Endpoint      string   `json:"endpoint"`
}
