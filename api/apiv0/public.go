package apiv0

// Public roots (backlog 0043): content the operator chose to serve on the public HTTPS
// listener. Marking is explicit per root (operations.md P2) and protects the root like a
// pin.

// PublicRequest marks a held root public.
type PublicRequest struct {
	CID string `json:"cid"`
}

// PublicRoot is one public root and, while the public listener runs, its URLs.
type PublicRoot struct {
	CID string `json:"cid"`
	// HTTPS is the bare-origin browser URL (listing or download), Site the site origin of
	// a tree, RASL the rasl:// link hinting this seed; all empty when no public listener
	// is running in this process.
	HTTPS string `json:"https,omitempty"`
	Site  string `json:"site,omitempty"`
	RASL  string `json:"rasl,omitempty"`
}
