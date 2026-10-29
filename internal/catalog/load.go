package catalog

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// indexName is the file name of the index in a catalog folder.
const indexName = "tools.yaml"

// indexVersion is the only version of tools.yaml this build reads.
const indexVersion = 1

// rawIndex is tools.yaml as written.
type rawIndex struct {
	Version *int                 `yaml:"version"`
	Tools   map[string]*rawEntry `yaml:"tools"`
}

// rawEntry is one tool of tools.yaml as written. An empty string is a key
// the tool does not set.
type rawEntry struct {
	Description string      `yaml:"description"`
	Mode        string      `yaml:"mode"`
	Databases   []string    `yaml:"databases"`
	Login       string      `yaml:"login"`
	Params      []*rawParam `yaml:"params"`
	Limits      *rawLimits  `yaml:"limits"`
}

type rawParam struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"`
	Enum        []string `yaml:"enum"`
	Pattern     string   `yaml:"pattern"`
	Minimum     *float64 `yaml:"minimum"`
	Maximum     *float64 `yaml:"maximum"`
	MaxLength   *int     `yaml:"maxLength"`
}

// rawLimits is the limits key of a tool as written. A nil field keeps the
// cap of server.yaml.
type rawLimits struct {
	Rows      *int    `yaml:"rows"`
	Bytes     *int    `yaml:"bytes"`
	Timeout   *string `yaml:"timeout"`
	WriteRows *int    `yaml:"write_rows"`
}

// folder is one folder of the catalog: the statements of its .sql files and
// its index.
type folder struct {
	statements []*statement
	// index is the folder's tools.yaml, or nil when the folder has none.
	index *rawIndex
}

// Load reads the catalog folder at dir: every .sql file and tools.yaml at
// any depth, skipping names that start with a dot, and following links only
// where they stay inside dir. A tool uses the statement of its name from the
// .sql files of its own folder. databases are the databases of server.yaml,
// and reserved are the built-in tool names.
//
// Load returns the entries folder by folder and by name within a folder,
// with Source relative to dir and every Param.Type empty, and a report for
// each statement that no tools.yaml lists. It fails on the first file it
// cannot read or parse, a link that leads outside dir, a statement name used
// twice in the catalog, and a tool without a statement, with a reserved
// name, or naming a database or login that server.yaml does not define.
func Load(dir string, databases map[string]config.Database, reserved []string) ([]Entry, []string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, errors.New("adaptive-pg-mcp cannot read the catalog folder " + dir + ": " + err.Error() + ". Create it, correct catalog in server.yaml, or set catalog: none.")
	}
	if !info.IsDir() {
		return nil, nil, errors.New("the catalog path " + dir + " is a file, not a folder. Point catalog at the folder that holds the .sql files, or set catalog: none.")
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, errors.New("adaptive-pg-mcp cannot read the catalog folder " + dir + ": " + err.Error() + ". Create it, correct catalog in server.yaml, or set catalog: none.")
	}
	folders, order, all, err := walk(root, dir)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]*statement{}
	for _, s := range all {
		if first, dup := seen[s.name]; dup {
			return nil, nil, errors.New(first.file + " line " + strconv.Itoa(first.line) + " and " + s.file + " line " + strconv.Itoa(s.line) + " both name a statement " + s.name + ". Rename one of them in its -- name: line.")
		}
		seen[s.name] = s
	}
	var entries []Entry
	for _, rel := range order {
		f := folders[rel]
		if f.index == nil {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(f.index.Tools)) {
			e, err := join(path.Join(rel, indexName), name, f.index.Tools[name], f.statements, databases, reserved)
			if err != nil {
				return nil, nil, err
			}
			entries = append(entries, e)
		}
	}
	var reports []string
	for _, s := range all {
		if !s.listed {
			reports = append(reports, s.file+" statement "+s.name+" is not a tool, because "+path.Join(path.Dir(s.file), indexName)+" does not list it. List it there to serve it.")
		}
	}
	return entries, reports, nil
}

// walk reads every .sql file and tools.yaml under root, which is the catalog
// folder dir with its symbolic links resolved. It skips every name that
// starts with a dot, such as .git or the timestamped folder of a Kubernetes
// ConfigMap mount. It reads through an os.Root, which follows only relative
// links that never climb above root, so a link to a folder, a .sql file or
// tools.yaml that breaks this, at any step, fails the load. Links to anything
// else are ignored. Each folder is read once, by its real path.
//
// walk returns the folders by their slash path relative to root, with . for
// root, those paths in walk order, and every statement in that order.
// Messages name the folder as dir.
func walk(root, dir string) (map[string]*folder, []string, []*statement, error) {
	inside, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, nil, errors.New("adaptive-pg-mcp cannot read the catalog folder " + dir + ": " + err.Error() + ". Create it, correct catalog in server.yaml, or set catalog: none.")
	}
	defer inside.Close()
	w := &walker{root: root, dir: dir, inside: inside, read: map[string]bool{}, folders: map[string]*folder{}}
	err = w.folder(".", root)
	return w.folders, w.order, w.all, err
}

// walker is one walk of the catalog folder.
type walker struct {
	// root is the catalog folder with its links resolved, and dir the folder
	// as server.yaml names it.
	root, dir string
	inside    *os.Root
	// read holds the real path of every folder the walk has read.
	read    map[string]bool
	folders map[string]*folder
	order   []string
	all     []*statement
}

// unreadable returns the error for a file or folder at rel that the walk
// cannot read.
func (w *walker) unreadable(rel, kind string, err error) error {
	return errors.New("adaptive-pg-mcp cannot read " + rel + " in the catalog folder " + w.dir + ": " + err.Error() + ". Make it " + kind + " that adaptive-pg-mcp can read, or remove it.")
}

// folder reads the folder at rel, whose real path is real, unless the walk
// has read it already, and then each file and folder in it, by name.
func (w *walker) folder(rel, real string) error {
	if w.read[real] {
		return nil
	}
	w.read[real] = true
	w.folders[rel] = &folder{}
	w.order = append(w.order, rel)
	f, err := w.inside.Open(filepath.FromSlash(rel))
	if err != nil {
		return w.unreadable(rel, "a folder", err)
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return w.unreadable(rel, "a folder", err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		child := path.Join(rel, name)
		switch {
		case e.IsDir():
			err = w.folder(child, filepath.Join(real, name))
		case e.Type()&fs.ModeSymlink != 0:
			err = w.link(child, name, real)
		default:
			err = w.file(child, name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// link reads the target of the link at rel when it is a folder or a file
// the walk reads, and ignores it otherwise. parent is the real path of the
// folder that holds the link. A link that os.Root would not follow is
// refused before any read.
func (w *walker) link(rel, name, parent string) error {
	full := filepath.Join(w.root, filepath.FromSlash(rel))
	if name != indexName && !strings.HasSuffix(name, ".sql") {
		if info, err := os.Stat(full); err != nil || !info.IsDir() {
			return nil
		}
	}
	target, err := os.Readlink(full)
	if err != nil {
		return w.unreadable(rel, "a file", err)
	}
	if filepath.IsAbs(target) {
		return errors.New(rel + " is a link to an absolute path. Make it a relative link inside the catalog folder " + w.dir + ", or remove it.")
	}
	if w.climbs(parent, target) {
		return errors.New(rel + " is a link that climbs above the catalog folder " + w.dir + ". Make it a relative link inside the folder, or remove it.")
	}
	info, err := w.inside.Stat(filepath.FromSlash(rel))
	if err != nil {
		if _, missing := filepath.EvalSymlinks(full); missing != nil {
			return w.unreadable(rel, "a file", missing)
		}
		return errors.New(rel + " is a link through another link that is absolute or leaves the catalog folder " + w.dir + ". Make each link on the way relative and inside the folder, or remove it.")
	}
	if !info.IsDir() {
		return w.file(rel, name)
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return w.unreadable(rel, "a folder", err)
	}
	return w.folder(rel, real)
}

// climbs reports whether the relative link target, read from the folder
// whose real path is parent, climbs above root at any step. It counts a path
// that comes back in later, because os.Root refuses it too.
func (w *walker) climbs(parent, target string) bool {
	depth := 0
	if r, err := filepath.Rel(w.root, parent); err == nil && r != "." {
		depth = len(strings.Split(filepath.ToSlash(r), "/"))
	}
	for _, part := range strings.Split(filepath.ToSlash(target), "/") {
		switch part {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}

// file reads the file at rel when it is a .sql file or tools.yaml, and adds
// what it holds to its folder.
func (w *walker) file(rel, name string) error {
	if name != indexName && !strings.HasSuffix(name, ".sql") {
		return nil
	}
	data, err := w.inside.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		return w.unreadable(rel, "a file", err)
	}
	f := w.folders[path.Dir(rel)]
	if name == indexName {
		var raw rawIndex
		if err := config.Decode(rel, data, indexVersion, &raw); err != nil {
			return err
		}
		f.index = &raw
		return nil
	}
	statements, err := splitStatements(rel, string(data))
	if err != nil {
		return err
	}
	f.statements = append(f.statements, statements...)
	w.all = append(w.all, statements...)
	return nil
}

// join builds the Entry for the tool name of the tools.yaml at index from
// the statement of that name among statements, the statements of its
// folder, and checks it against databases and reserved.
func join(index, name string, raw *rawEntry, statements []*statement, databases map[string]config.Database, reserved []string) (Entry, error) {
	at := index + " tool " + name
	var st *statement
	for _, s := range statements {
		if s.name == name {
			st = s
		}
	}
	if st == nil {
		return Entry{}, errors.New(at + " has no statement in the .sql files of its folder. Add a -- name: " + name + " line above its statement in a .sql file next to " + index + ", or remove the tool.")
	}
	if slices.Contains(reserved, name) {
		return Entry{}, errors.New(at + " has the name of a built-in tool. Rename it, and its -- name: line in " + st.file + ".")
	}
	if raw == nil {
		raw = &rawEntry{}
	}
	e := Entry{Name: name, Description: raw.Description, Databases: raw.Databases, Login: raw.Login, SQL: st.sql, Source: st.file}
	switch raw.Mode {
	case "read":
		e.Mode = Read
	case "write":
		e.Mode = Write
	case "":
		return Entry{}, errors.New(at + " does not set mode. Set mode to read or write.")
	default:
		return Entry{}, errors.New(at + " has mode " + raw.Mode + ". Set mode to read or write.")
	}
	if len(raw.Databases) == 0 {
		return Entry{}, errors.New(at + " lists no databases. List the databases it runs on, by their names in server.yaml.")
	}
	if raw.Login == "" {
		return Entry{}, errors.New(at + " does not set login. Set login to read, write or another login of its databases.")
	}
	for _, db := range raw.Databases {
		d, ok := databases[db]
		if !ok {
			return Entry{}, errors.New(at + " names the database " + db + ", which server.yaml does not define. Remove " + db + " from its databases, or add it to databases in server.yaml.")
		}
		if _, ok := d.Logins[raw.Login]; !ok {
			return Entry{}, errors.New(at + " uses the login " + raw.Login + ", which " + db + " does not have in server.yaml. Add " + raw.Login + " to the logins of " + db + ", or set login to one it has, such as read.")
		}
	}
	limits, err := limitsOf(at, raw.Limits)
	if err != nil {
		return Entry{}, err
	}
	e.Limits = limits
	for _, p := range raw.Params {
		if p == nil {
			p = &rawParam{}
		}
		e.Params = append(e.Params, Param{
			Name: p.Name, Description: p.Description, Enum: p.Enum, Pattern: p.Pattern,
			Minimum: p.Minimum, Maximum: p.Maximum, MaxLength: p.MaxLength, Required: p.Required,
		})
	}
	st.listed = true
	return e, nil
}

// limitsOf checks the caps of the tool at, and returns nil when it sets
// none.
func limitsOf(at string, raw *rawLimits) (*Limits, error) {
	if raw == nil {
		return nil, nil
	}
	l := &Limits{}
	for _, c := range []struct {
		key string
		v   *int
		dst *int
	}{{"rows", raw.Rows, &l.Rows}, {"bytes", raw.Bytes, &l.Bytes}, {"write_rows", raw.WriteRows, &l.WriteRows}} {
		if c.v == nil {
			continue
		}
		if *c.v < 1 {
			return nil, errors.New(at + " has limits." + c.key + " " + strconv.Itoa(*c.v) + ". Set it to 1 or more, or remove it to keep the cap of server.yaml.")
		}
		*c.dst = *c.v
	}
	if raw.Timeout != nil {
		d, err := time.ParseDuration(*raw.Timeout)
		if err != nil {
			return nil, errors.New(at + " has limits.timeout " + *raw.Timeout + ", which is not a duration. Write it with a unit, such as 30s.")
		}
		if d <= 0 {
			return nil, errors.New(at + " has limits.timeout " + *raw.Timeout + ". Set it above zero, such as 30s, or remove it to keep the timeout of server.yaml.")
		}
		l.Timeout = d
	}
	return l, nil
}
