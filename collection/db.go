package collection

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// The collection state lives in its own SQLite database beside the cluster state
// (`$NU_PATH/cluster/net/collections.db`): merged catalog per collection, the item table
// (change stream and facet index), conflicts, local rules and manual holds, this node's
// holdings and holdings log, and the holdings index.
var schema = []string{
	`CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
	CREATE TABLE coll (ds TEXT PRIMARY KEY, root BLOB, size INTEGER NOT NULL DEFAULT 0, header BLOB,
		writers TEXT NOT NULL DEFAULT '[]', drops TEXT NOT NULL DEFAULT '[]', item_seq INTEGER NOT NULL DEFAULT 0,
		eval_ms INTEGER NOT NULL DEFAULT 0, rules_gen INTEGER NOT NULL DEFAULT 0, eval_gen INTEGER NOT NULL DEFAULT -1,
		facets TEXT NOT NULL DEFAULT '{}', synced INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE items (ds TEXT NOT NULL, path TEXT NOT NULL, entry BLOB, kind TEXT, size INTEGER NOT NULL DEFAULT 0,
		meta BLOB, writer TEXT, facets TEXT, rule INTEGER NOT NULL DEFAULT -1, seq INTEGER NOT NULL, deleted INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (ds, path)) WITHOUT ROWID;
	CREATE INDEX items_seq ON items (ds, seq);
	CREATE INDEX items_rule ON items (ds, rule, deleted);
	CREATE TABLE ftime (ds TEXT NOT NULL, facet TEXT NOT NULL, path TEXT NOT NULL, ms INTEGER NOT NULL,
		PRIMARY KEY (ds, facet, path)) WITHOUT ROWID;
	CREATE INDEX ftime_ms ON ftime (ds, facet, ms);
	CREATE TABLE conflicts (ds TEXT NOT NULL, path TEXT NOT NULL, loser TEXT NOT NULL, winner TEXT, entry BLOB NOT NULL,
		reason TEXT NOT NULL, PRIMARY KEY (ds, path, loser)) WITHOUT ROWID;
	CREATE TABLE rules (ds TEXT PRIMARY KEY, json TEXT NOT NULL);
	CREATE TABLE manual (ds TEXT NOT NULL, path TEXT NOT NULL, PRIMARY KEY (ds, path)) WITHOUT ROWID;
	CREATE TABLE holdings (ds TEXT NOT NULL, path TEXT NOT NULL, entry BLOB NOT NULL, state TEXT NOT NULL, src TEXT NOT NULL,
		rule INTEGER NOT NULL DEFAULT -1, bytes INTEGER NOT NULL DEFAULT 0, tries INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (ds, path)) WITHOUT ROWID;
	CREATE INDEX holdings_state ON holdings (ds, state);
	CREATE TABLE hlog (ds TEXT NOT NULL, seq INTEGER NOT NULL, path TEXT NOT NULL, entry BLOB, state TEXT NOT NULL,
		src TEXT NOT NULL DEFAULT '', bytes INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ds, seq)) WITHOUT ROWID;
	CREATE TABLE idx (ds TEXT NOT NULL, peer TEXT NOT NULL, path TEXT NOT NULL, entry BLOB, state TEXT NOT NULL,
		src TEXT NOT NULL DEFAULT '', bytes INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ds, peer, path)) WITHOUT ROWID;
	CREATE INDEX idx_path ON idx (ds, path, state);
	CREATE TABLE idx_cursor (ds TEXT NOT NULL, peer TEXT NOT NULL, epoch TEXT NOT NULL, seq INTEGER NOT NULL,
		PRIMARY KEY (ds, peer)) WITHOUT ROWID;
	CREATE TABLE pending (ds TEXT NOT NULL, path TEXT NOT NULL, entry BLOB, PRIMARY KEY (ds, path)) WITHOUT ROWID;`,
	// 0059: item pushes (the durable accept queue and push log).
	`CREATE TABLE pushes (ds TEXT NOT NULL, path TEXT NOT NULL, id TEXT NOT NULL, digest BLOB NOT NULL, req TEXT NOT NULL,
		state TEXT NOT NULL, replace INTEGER NOT NULL DEFAULT 0, files TEXT NOT NULL DEFAULT '{}', sources TEXT NOT NULL DEFAULT '{}',
		entry BLOB, size INTEGER NOT NULL DEFAULT 0, meta BLOB, head BLOB, error TEXT, tries INTEGER NOT NULL DEFAULT 0,
		next_ms INTEGER NOT NULL DEFAULT 0, created_ms INTEGER NOT NULL, updated_ms INTEGER NOT NULL, seq INTEGER NOT NULL,
		PRIMARY KEY (ds, path)) WITHOUT ROWID;
	CREATE INDEX pushes_seq ON pushes (ds, seq);
	CREATE INDEX pushes_work ON pushes (state, next_ms);`,
}

func openDB(path string) (*sql.DB, string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, "", err
	}
	db.SetMaxOpenConns(1)
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		db.Close()
		return nil, "", err
	}
	if v > len(schema) {
		db.Close()
		return nil, "", fmt.Errorf("collection: database schema %d is newer than this build", v)
	}
	for ; v < len(schema); v++ {
		if _, err := db.Exec(schema[v]); err != nil {
			db.Close()
			return nil, "", fmt.Errorf("collection: migrate to %d: %w", v+1, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			db.Close()
			return nil, "", err
		}
	}
	// The epoch names this database's holdings logs and item stream: a recreated database
	// gets a new one, so cursors into the old one answer resync.
	var epoch string
	err = db.QueryRow(`SELECT v FROM meta WHERE k = 'epoch'`).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		epoch = hex.EncodeToString(b)
		_, err = db.Exec(`INSERT INTO meta (k, v) VALUES ('epoch', ?)`, epoch)
	}
	if err != nil {
		db.Close()
		return nil, "", err
	}
	return db, epoch, nil
}
