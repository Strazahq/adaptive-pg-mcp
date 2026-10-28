package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// decode reads the YAML in data into out after checking its version, and
// refuses any key that out does not have.
func decode(path string, data []byte, out any) error {
	var head struct {
		Version *int `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return decodeError(path, err)
	}
	if head.Version == nil {
		return fmt.Errorf("%s does not set version, which names the format the file is written in. Add version: %d.", path, formatVersion)
	}
	if *head.Version != formatVersion {
		return fmt.Errorf("%s has version %d, and this build reads version %d only. Write the file in version %d, or run a build that reads version %d.", path, *head.Version, formatVersion, formatVersion, *head.Version)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return decodeError(path, err)
	}
	return nil
}

var (
	// unknownKey matches the decoder's message for a key the target type does not have.
	unknownKey = regexp.MustCompile(`^line (\d+): field (.+) not found in type `)
	// repeatedKey matches the decoder's messages for a key written twice in one mapping.
	repeatedKey = regexp.MustCompile(`^line (\d+): (?:field (.+) already set in type |mapping key "(.+)" already defined at line )`)
	// wrongType matches the decoder's message for a value of the wrong type, whose value it never repeats.
	wrongType = regexp.MustCompile(`^line (\d+): cannot unmarshal .* into (.+)$`)
	// anyLine matches the line number at the start of any other decoder message.
	anyLine = regexp.MustCompile(`^line (\d+): `)
)

// decodeError turns a decoder error into a sentence. It never repeats a value
// from the file, so a secret written in the wrong place stays out of the log.
func decodeError(path string, err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) || len(te.Errors) == 0 {
		return fmt.Errorf("%s is not valid YAML: %v. Correct the syntax on the line it names.", path, err)
	}
	e := te.Errors[0]
	if m := unknownKey.FindStringSubmatch(e); m != nil {
		return fmt.Errorf("%s line %s has the key %s, which this build does not read. Remove it, or correct its spelling.", path, m[1], m[2])
	}
	if m := repeatedKey.FindStringSubmatch(e); m != nil {
		return fmt.Errorf("%s line %s sets the key %s a second time in one place. Keep one of the two.", path, m[1], m[2]+m[3])
	}
	if m := wrongType.FindStringSubmatch(e); m != nil {
		return fmt.Errorf("%s line %s has a value of the wrong type, where %s belongs. Write %s there.", path, m[1], kind(m[2]), kind(m[2]))
	}
	if m := anyLine.FindStringSubmatch(e); m != nil {
		return fmt.Errorf("%s line %s has a value adaptive-pg-mcp cannot read. Compare that line with the example file.", path, m[1])
	}
	return fmt.Errorf("%s has a value adaptive-pg-mcp cannot read. Compare it with the example file.", path)
}

// kind names the YAML a Go type takes, in plain words.
func kind(goType string) string {
	t := strings.TrimLeft(goType, "*")
	switch {
	case t == "int":
		return "a whole number"
	case t == "string":
		return "a single value"
	case t == "bool":
		return "true or false"
	case strings.HasPrefix(t, "[]"):
		return "a list"
	default:
		return "a mapping of keys"
	}
}
