package config

import (
	"fmt"
	"os"
)

// rawAccess is access.yaml as written. A nil field is a key the file does
// not set.
type rawAccess struct {
	Version *int         `yaml:"version"`
	Callers []*rawCaller `yaml:"callers"`
}

type rawCaller struct {
	Name        *string   `yaml:"name"`
	TokenSHA256 *string   `yaml:"token_sha256"`
	Tools       *[]string `yaml:"tools"`
	Databases   *[]string `yaml:"databases"`
	Tables      []string  `yaml:"tables"`
}

// callerKeys follows the key in every missing caller key refusal.
const callerKeys = "a required key"

// loadAccess reads access.yaml at path and checks every caller against the
// databases of server.yaml.
func loadAccess(path string, databases map[string]Database) ([]Caller, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("adaptive-pg-mcp cannot read access.yaml at %s: %v. Create the file, or correct access in server.yaml.", path, err)
	}
	var raw rawAccess
	if err := decode(path, data, &raw); err != nil {
		return nil, err
	}
	if len(raw.Callers) == 0 {
		return nil, fmt.Errorf("%s does not set callers. Add at least one caller.", path)
	}
	names := map[string]bool{}
	owners := map[Hash]string{}
	callers := make([]Caller, 0, len(raw.Callers))
	for i, rc := range raw.Callers {
		if rc == nil || unset(rc.Name) {
			return nil, fmt.Errorf("%s caller %d does not set name, %s. Add name: a unique name for the caller.", path, i+1, callerKeys)
		}
		name := *rc.Name
		if names[name] {
			return nil, fmt.Errorf("%s lists the caller %s twice. Give each caller a unique name.", path, name)
		}
		names[name] = true
		if unset(rc.TokenSHA256) {
			return nil, fmt.Errorf("%s caller %s does not set token_sha256, %s. Add the token_sha256 that adaptive-pg-mcp token prints.", path, name, callerKeys)
		}
		hash, ok := parseHash(*rc.TokenSHA256)
		if !ok {
			return nil, fmt.Errorf("%s caller %s has a token_sha256 that is not 64 lowercase hex characters. Copy it as adaptive-pg-mcp token prints it.", path, name)
		}
		if other, dup := owners[hash]; dup {
			return nil, fmt.Errorf("%s gives the callers %s and %s the same token_sha256. Give each caller its own token.", path, other, name)
		}
		owners[hash] = name
		if rc.Tools == nil {
			return nil, fmt.Errorf("%s caller %s does not set tools, %s. Add tools: the tools it may use, such as [list_tables, read_rows].", path, name, callerKeys)
		}
		if rc.Databases == nil {
			return nil, fmt.Errorf("%s caller %s does not set databases, %s. Add databases: the databases it may use, such as [helpdesk].", path, name, callerKeys)
		}
		for _, db := range *rc.Databases {
			if _, ok := databases[db]; !ok {
				return nil, fmt.Errorf("%s caller %s names the database %s, which server.yaml does not define. Remove %s from the caller, or add it to databases in server.yaml.", path, name, db, db)
			}
		}
		callers = append(callers, Caller{
			Name:        name,
			TokenSHA256: hash,
			Tools:       *rc.Tools,
			Databases:   *rc.Databases,
			Tables:      rc.Tables,
		})
	}
	return callers, nil
}
