package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Pin is one entry of a Kubo pin inventory.
type Pin struct {
	CID  string `json:"cid"`
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// MaxPinInventory bounds a pin inventory file.
const MaxPinInventory = 64 << 20

// ParsePins reads the output of `ipfs pin ls --type recursive --enc=json` (one
// {"Keys":{cid:{Type,Name}}} object, with or without --names) or its --stream form (one
// {"Cid","Type","Name"} object per line). Entries are returned sorted by CID; their CIDs are
// not yet validated, so a bad entry is reported per root rather than failing the batch.
func ParsePins(r io.Reader) ([]Pin, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxPinInventory+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxPinInventory {
		return nil, fmt.Errorf("bridge: pin inventory exceeds %d bytes", MaxPinInventory)
	}
	var pins []Pin
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var v struct {
			Keys map[string]struct{ Type, Name string }
			Cid  string
			Type string
			Name string
		}
		if err := dec.Decode(&v); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("bridge: pin inventory is not `ipfs pin ls --enc=json` output: %w", err)
		}
		for c, k := range v.Keys {
			pins = append(pins, Pin{CID: c, Type: k.Type, Name: k.Name})
		}
		if v.Cid != "" {
			pins = append(pins, Pin{CID: v.Cid, Type: v.Type, Name: v.Name})
		}
	}
	if len(pins) == 0 {
		return nil, errors.New("bridge: pin inventory lists no pins")
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].CID < pins[j].CID })
	return pins, nil
}
