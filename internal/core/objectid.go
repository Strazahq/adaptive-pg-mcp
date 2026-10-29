package core

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// objectIDKey is the _meta key in which Straza sends the id of the database
// a call names.
const objectIDKey = "ai.straza/object-id"

// idSQL reads the system identifier of the cluster of the connection and
// the OID of its database, joined by a colon. OIDs repeat across clusters,
// and a database dropped and created again under the same name gets a new
// OID, so the pair names one database once. readID adds the config name.
const idSQL = `SELECT s.system_identifier::text || ':' || d.oid::text
FROM pg_control_system() s, pg_database d
WHERE d.datname = current_database()`

// errNotStarted is the error of a database call made before the start checks
// opened the pools.
var errNotStarted = errors.New("adaptive-pg-mcp has not run its start checks, so it has no connection to the database")

// readID reads the id of database on tx: what idSQL reads, a colon and the
// config name, because a standby or a restored copy of a database has the
// same system identifier and OID, and two config names must not share an id.
func readID(ctx context.Context, tx *pg.Tx, database string) (string, error) {
	rows, err := tx.Query(ctx, idSQL)
	if err != nil {
		return "", err
	}
	var id string
	if err := exactlyOne(rows, &id); err != nil {
		return "", err
	}
	return id + ":" + database, nil
}

// readIDOn reads the id of the database of login l in a read-only
// transaction of its own.
func (s *Server) readIDOn(ctx context.Context, l pg.Login) (string, error) {
	if s.pools == nil {
		return "", errNotStarted
	}
	var id string
	err := s.pools.Run(ctx, l, pg.ReadOnly, s.cfg.Limits.Timeout, func(tx *pg.Tx) error {
		var err error
		id, err = readID(ctx, tx, l.Database)
		return err
	})
	return id, err
}

// checkObjectID is step 4 of every call in docs/DESIGN.md section 5. When
// meta carries ai.straza/object-id, it must equal the id of database read on
// tx, the transaction the call runs in. A call without the key passes,
// because Straza sends it only for a role that names databases one by one.
func (s *Server) checkObjectID(ctx context.Context, tx *pg.Tx, database string, meta mcp.Meta) error {
	v, ok := meta[objectIDKey]
	if !ok {
		return nil
	}
	sent, isString := v.(string)
	if !isString || sent == "" {
		return errors.New("the " + objectIDKey + " of the call is empty or not a string. Send the call through Straza, which sets it from list_databases.")
	}
	id, err := readID(ctx, tx, database)
	if err != nil {
		return errors.New("the id of " + database + " cannot be read to compare with the " + objectIDKey + " of the call, because " + s.pools.Reason(err) + ". Try again, and if it fails again, run adaptive-pg-mcp check to see why.")
	}
	if id != sent {
		return errors.New("the id of " + database + " differs from the one Straza listed: the database was recreated, or server.yaml now gives its name to another database. Try again after Straza lists the databases again, or ask an administrator to refresh the list.")
	}
	return nil
}
