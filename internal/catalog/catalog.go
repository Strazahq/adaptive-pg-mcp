// Package catalog loads and checks the catalog: reviewed SQL statements that
// tools.yaml files turn into tools.
package catalog

import "time"

// Mode says whether a catalog tool reads or writes. The zero Mode is
// neither, so a tool without a mode is refused.
type Mode int

const (
	// Read runs the statement in a read-only transaction.
	Read Mode = iota + 1
	// Write runs the statement in a read-write transaction under the write cap.
	Write
)

// Entry is one catalog tool: a statement and its settings in tools.yaml.
type Entry struct {
	// Name is the tool name, from the statement's -- name: line.
	Name string
	// Description tells the agent what the tool does, what it returns and
	// its caps.
	Description string
	// Mode says whether the statement reads or writes.
	Mode Mode
	// Databases are the config names of the databases the tool runs on.
	Databases []string
	// Login names the login the statement runs as, such as read, write or
	// finance_reader.
	Login string
	// SQL is the statement text from its .sql file.
	SQL string
	// Params are the statement's parameters in $1 order.
	Params []Param
	// Limits are the tool's own caps. Nil keeps the caps of server.yaml.
	Limits *Limits
	// Source is the .sql file relative to the catalog folder, for messages.
	Source string
}

// Param is one statement parameter, with the constraints tools.yaml sets.
type Param struct {
	// Name is the argument name the agent passes the value under.
	Name string
	// Description tells the agent what to pass.
	Description string
	// Type is the Postgres type name that the prepare reports, such as int4.
	// A domain reads as its base type.
	Type string
	// Enum lists the only values the parameter takes. Empty means any.
	Enum []string
	// Pattern is a regular expression a text value must match.
	Pattern string
	// Minimum is the smallest number the parameter takes.
	Minimum *float64
	// Maximum is the largest number the parameter takes.
	Maximum *float64
	// MaxLength is the longest text the parameter takes, in characters.
	MaxLength *int
	// Required says the agent must pass the parameter.
	Required bool
}

// Limits are a catalog tool's own caps. A zero field keeps the cap of
// server.yaml.
type Limits struct {
	// Rows is the most rows a read returns.
	Rows int
	// Bytes is the most bytes a read returns.
	Bytes int
	// Timeout is the statement_timeout of each call.
	Timeout time.Duration
	// WriteRows is the most rows a write may change. A write over it rolls
	// back.
	WriteRows int
}
