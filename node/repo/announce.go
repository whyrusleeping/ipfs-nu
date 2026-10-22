package repo

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
)

// NoAnnounceFile lists the roots pinned with `--no-announce` (a JSON array of CID
// strings, 0600). The DHT provider loop skips them; an explicit announced pin or an
// unpin removes the entry. This is the sharing table's "unlisted", not access control.
const NoAnnounceFile = "no-announce.json"

var noAnnounceMu sync.Mutex

// NoAnnounced returns the roots marked unannounced.
func (r *Repo) NoAnnounced() ([]string, error) {
	noAnnounceMu.Lock()
	defer noAnnounceMu.Unlock()
	return r.readNoAnnounce()
}

func (r *Repo) readNoAnnounce() ([]string, error) {
	b, err := os.ReadFile(r.Path(NoAnnounceFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetNoAnnounce marks or unmarks a root (CID string) as unannounced.
func (r *Repo) SetNoAnnounce(c string, on bool) error {
	noAnnounceMu.Lock()
	defer noAnnounceMu.Unlock()
	list, err := r.readNoAnnounce()
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(list)+1)
	for _, s := range list {
		set[s] = true
	}
	if set[c] == on {
		return nil
	}
	if on {
		set[c] = true
	} else {
		delete(set, c)
	}
	list = list[:0]
	for s := range set {
		list = append(list, s)
	}
	sort.Strings(list)
	if len(list) == 0 {
		return r.RemoveAddrFile(NoAnnounceFile)
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return writeFileAtomic(r.Path(NoAnnounceFile), b)
}
