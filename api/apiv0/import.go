package apiv0

// IPFSImportResult describes verified source-to-Nu conversion.
type IPFSImportResult struct {
	IPFSRoot string `json:"ipfs_root"`
	Root     string `json:"root"`
	Manifest string `json:"manifest"`
	Files    uint64 `json:"files"`
	Bytes    uint64 `json:"bytes"`
}
