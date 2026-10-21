package gateway

import "context"

// atproto snapshots (D21 "Using atproto data"; contract G5: /at/… is inert).

// AtSnapshot is a held, verified repository snapshot.
type AtSnapshot struct {
	DID    string
	Rev    string
	Commit string
}

// AtCollection is one record collection of a snapshot.
type AtCollection struct {
	Name  string
	Count int
}

// AtRecord is one record of a collection.
type AtRecord struct {
	RKey string
	CID  string
	Size int64
}

// AtContent is the optional snapshot view of a Content. The gateway serves /at/ routes
// only when its Content implements it; it never fetches a live repository.
type AtContent interface {
	// AtSnapshot returns the held snapshot of did at rev ("" for the latest), or
	// apiv0.CodeNotFound.
	AtSnapshot(ctx context.Context, did, rev string) (*AtSnapshot, error)
	// AtCollections lists the snapshot's collections in name order.
	AtCollections(ctx context.Context, s *AtSnapshot) ([]AtCollection, error)
	// AtRecords lists a collection's records in rkey order.
	AtRecords(ctx context.Context, s *AtSnapshot, collection string) ([]AtRecord, error)
	// AtRecordJSON returns one record as atproto-style JSON and its CID.
	AtRecordJSON(ctx context.Context, s *AtSnapshot, collection, rkey string) ([]byte, string, error)
}
