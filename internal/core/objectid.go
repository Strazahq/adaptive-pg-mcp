package core

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/pg"
)

// objectIDKey is the _meta key in which Straza sends the id of the database a call names.
const objectIDKey = "ai.straza/object-id"

// idSQL reads the id of the database of the connection: the system
// identifier of its cluster and its OID, joined by a colon. OIDs repeat
// across clusters, and a database dropped and created again under the same
// name gets a new OID, so the pair names one database once.
const idSQL = `SELECT s.system_identifier::text || ':' || d.oid::text
FROM pg_control_system() s, pg_database d
WHERE d.datname = current_database()`

// errNotStarted refuses a database call on a server whose start checks have not opened the pools.
var errNotStarted = errors.New("adaptive-pg-mcp has not run its start checks, so it has no connection to the database")

// readID reads the id of the database on tx.
func readID(ctx context.Context, tx *pg.Tx) (string, error) {
	rows, err := tx.Query(ctx, idSQL)
	if err != nil {
		return "", err
	}
	var id string
	return id, exactlyOne(rows, &id)
}

// readIDOn reads the id of the database of login l in a read-only transaction of its own.
func (s *Server) readIDOn(ctx context.Context, l pg.Login) (string, error) {
	if s.pools == nil {
		return "", errNotStarted
	}
	var id string
	err := s.pools.Run(ctx, l, pg.ReadOnly, s.cfg.Limits.Timeout, func(tx *pg.Tx) error {
		var err error
		id, err = readID(ctx, tx)
		return err
	})
	return id, err
}

// checkObjectID is step 4 of every call in docs/DESIGN.md section 5. When
// meta carries an ai.straza/object-id, it must equal the id of database read
// on tx, the transaction the call runs in, and otherwise the call is refused.
// A call without the key passes, because Straza sends it only for a role that
// names databases one by one.
func (s *Server) checkObjectID(ctx context.Context, tx *pg.Tx, database string, meta mcp.Meta) error {
	v, ok := meta[objectIDKey]
	if !ok {
		return nil
	}
	sent, isString := v.(string)
	if !isString || sent == "" {
		return errors.New("the call carries an " + objectIDKey + " that is empty or not a string, so the call is refused. Straza sends the id that list_databases answered as a string, so send the call through Straza.")
	}
	id, err := readID(ctx, tx)
	if err != nil {
		return errors.New("adaptive-pg-mcp cannot read the id of " + database + " to compare it with the " + objectIDKey + " of the call, because " + s.pools.Reason(err) + ", so the call is refused. Try again, and if it fails again, run adaptive-pg-mcp check to see why the login cannot read it.")
	}
	if id != sent {
		return errors.New(database + " was dropped and created again after Straza listed it, so the call is refused. Try again once Straza lists the databases again, or ask an administrator to refresh the list.")
	}
	return nil
}
