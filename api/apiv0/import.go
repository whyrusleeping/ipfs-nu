package apiv0

// IPFSImportResult describes verified source-to-Nu conversion.
type IPFSImportResult struct {
	IPFSRoot                 string `json:"ipfs_root"`
	Root                     string `json:"root"`
	Manifest                 string `json:"manifest"`
	Files                    uint64 `json:"files"`
	Bytes                    uint64 `json:"bytes"`
	DryRun                   bool   `json:"dry_run,omitempty"`
	Blocks                   int    `json:"blocks,omitempty"`
	EstimatedPhysicalBytes   uint64 `json:"estimated_blob_physical_bytes,omitempty"`
	StoreHeadroom            int64  `json:"store_headroom,omitempty"`
	BlobEstimateFitsHeadroom *bool  `json:"blob_estimate_fits_headroom,omitempty"`
}
