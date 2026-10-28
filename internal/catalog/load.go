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

// indexName is the name of the index file of a catalog folder.
const indexName = "tools.yaml"

// indexVersion is the only version of tools.yaml this build reads.
const indexVersion = 1

// rawIndex is tools.yaml as written.
type rawIndex struct {
	Version *int                 `yaml:"version"`
	Tools   map[string]*rawEntry `yaml:"tools"`
}

// rawEntry is one entry of tools.yaml as written. An empty string is a key the entry does not set.
type rawEntry struct {
	Description string      `yaml:"description"`
	Mode        string      `yaml:"mode"`
	Databases   []string    `yaml:"databases"`
	Login       string      `yaml:"login"`
	Params      []*rawParam `yaml:"params"`
	Limits      *rawLimits  `yaml:"limits"`
}

// rawParam is one parameter of an entry as written.
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

// rawLimits is the limits key of an entry as written. A nil field keeps the limit of server.yaml.
type rawLimits struct {
	Rows      *int    `yaml:"rows"`
	Bytes     *int    `yaml:"bytes"`
	Timeout   *string `yaml:"timeout"`
	WriteRows *int    `yaml:"write_rows"`
}

// folder is one folder of the catalog: the statements of its .sql files and its index.
type folder struct {
	statements []*statement
	// index is the folder's tools.yaml, or nil when the folder has none.
	index *rawIndex
}

// Load reads the catalog folder at dir: every .sql file and every
// tools.yaml index in it, at any depth, except those whose name or folder
// starts with a dot, and through links only where they stay inside dir. It
// returns the entries that the indexes make tools, folder by folder and by
// name within a folder, with Source relative to dir and every Param's Type
// still empty, and a sentence for each statement that no index lists, which
// is not a tool. An entry joins the statement of its name in the .sql files
// of its own folder. databases are the databases of server.yaml, and
// reserved are the names of the built-in tools, which no entry may take.
// Load fails, with a sentence that names the file and the fix, on the first
// file it cannot read or parse, on a link that leads outside dir, on two
// statements with one name anywhere in the catalog, and on an entry without
// a statement, with a reserved name, or that names a database or a login
// that server.yaml does not define.
func Load(dir string, databases map[string]config.Database, reserved []string) ([]Entry, []string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, errors.New("adaptive-pg-mcp cannot read the catalog folder at " + dir + ", which server.yaml names in catalog: " + err.Error() + ". Create the folder, correct the path in catalog, or set catalog: none.")
	}
	if !info.IsDir() {
		return nil, nil, errors.New("the catalog folder at " + dir + ", which server.yaml names in catalog, is a file and not a folder. Point catalog at the folder that holds the .sql files and their tools.yaml, or set catalog: none.")
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, errors.New("adaptive-pg-mcp cannot read the catalog folder at " + dir + ", which server.yaml names in catalog: " + err.Error() + ". Create the folder, correct the path in catalog, or set catalog: none.")
	}
	folders, order, all, err := walk(root, dir)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]*statement{}
	for _, s := range all {
		if first, dup := seen[s.name]; dup {
			return nil, nil, errors.New(first.file + " line " + strconv.Itoa(first.line) + " and " + s.file + " line " + strconv.Itoa(s.line) + " both name a statement " + s.name + ", and a tool name must be unique in the catalog. Rename one of them in its -- name: line.")
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
			reports = append(reports, s.file+" statement "+s.name+" is not a tool, because "+path.Join(path.Dir(s.file), indexName)+" has no entry for it. Add an entry to serve it.")
		}
	}
	return entries, reports, nil
}

// walk reads every .sql file and tools.yaml under root, the catalog folder
// dir with its symbolic links resolved. It skips every file and folder whose
// name starts with a dot, at any depth, so that a hidden folder such as the
// timestamped folder of a Kubernetes ConfigMap mount, or .git, adds nothing.
// It reads through an os.Root of root, which follows a link only when the
// link is relative and its path never climbs above root, so a link that leads
// to a folder, a .sql file or tools.yaml must be such a link, through every
// link on its way, or the load fails, as invariant 6 asks. A link to anything
// else is ignored, as other files are. It reads each folder once, by its real
// path, however many links lead to it. It returns the folders by their
// path relative to root, with slashes and . for root itself, those paths in
// the order of the walk, and every statement in that order. A sentence names
// the folder as dir.
func walk(root, dir string) (map[string]*folder, []string, []*statement, error) {
	inside, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, nil, errors.New("adaptive-pg-mcp cannot read the catalog folder at " + dir + ", which server.yaml names in catalog: " + err.Error() + ". Create the folder, correct the path in catalog, or set catalog: none.")
	}
	defer inside.Close()
	w := &walker{root: root, dir: dir, inside: inside, read: map[string]bool{}, folders: map[string]*folder{}}
	err = w.folder(".", root)
	return w.folders, w.order, w.all, err
}

// walker is one walk of the catalog folder.
type walker struct {
	// root is the catalog folder with its links resolved, and dir the folder as server.yaml names it.
	root, dir string
	inside    *os.Root
	// read holds the real path of every folder the walk has read.
	read    map[string]bool
	folders map[string]*folder
	order   []string
	all     []*statement
}

// unreadable is the sentence about the file or folder at rel that the walk cannot read.
func (w *walker) unreadable(rel, kind string, err error) error {
	return errors.New("adaptive-pg-mcp cannot read " + rel + " in the catalog folder " + w.dir + ": " + err.Error() + ". Make it " + kind + " inside the catalog folder that the user who runs adaptive-pg-mcp can read, or remove it.")
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

// link reads what the link at rel leads to, when it is a folder, or a file
// the walk reads by its name, and ignores it otherwise. parent is the real
// path of the folder that holds the link. It refuses a link that os.Root
// would not follow, before any read, and says why.
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
		return errors.New(rel + " is a link to an absolute path, and adaptive-pg-mcp follows only relative links that stay inside the catalog folder " + w.dir + ". Make it a relative link to a path inside the folder, or remove it.")
	}
	if w.climbs(parent, target) {
		return errors.New(rel + " is a link whose path climbs above the catalog folder " + w.dir + ", and adaptive-pg-mcp follows only relative links that stay inside it. Make it a relative link to a path inside the folder, or remove it.")
	}
	info, err := w.inside.Stat(filepath.FromSlash(rel))
	if err != nil {
		if _, missing := filepath.EvalSymlinks(full); missing != nil {
			return w.unreadable(rel, "a file", missing)
		}
		return errors.New(rel + " is a link that leads through another link to an absolute path or above the catalog folder " + w.dir + ", and adaptive-pg-mcp follows only relative links that stay inside it. Make each link on the way relative and inside the folder, or remove it.")
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
// whose real path is parent, has a .. that climbs above root at any step,
// even when a later part of the path comes back in, since os.Root refuses it.
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

// file reads the file at rel when it is a .sql file or tools.yaml, and adds what it holds to its folder.
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

// join turns the entry name of the index at index into an Entry with the
// statement of that name among statements, the statements of the index's
// folder, and checks it against databases and reserved.
func join(index, name string, raw *rawEntry, statements []*statement, databases map[string]config.Database, reserved []string) (Entry, error) {
	at := index + " entry " + name
	var st *statement
	for _, s := range statements {
		if s.name == name {
			st = s
		}
	}
	if st == nil {
		return Entry{}, errors.New(at + " has no statement of that name in the .sql files of its folder, so it cannot be a tool. Add a -- name: " + name + " line before its statement in a .sql file next to " + index + ", or remove the entry.")
	}
	if slices.Contains(reserved, name) {
		return Entry{}, errors.New(at + " has the name of a built-in tool of adaptive-pg-mcp, and a tool name must be unique on the server. Rename the entry and its -- name: line in " + st.file + ".")
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
		return Entry{}, errors.New(at + " has mode " + raw.Mode + ", which is neither read nor write. Set mode to read or write.")
	}
	if len(raw.Databases) == 0 {
		return Entry{}, errors.New(at + " names no databases. List the databases it runs on, by their names in server.yaml.")
	}
	if raw.Login == "" {
		return Entry{}, errors.New(at + " does not set login, which names the login its statement runs on. Add login: read, login: write, or the name of another login of its databases.")
	}
	for _, db := range raw.Databases {
		d, ok := databases[db]
		if !ok {
			return Entry{}, errors.New(at + " names the database " + db + ", which server.yaml does not define. Remove " + db + " from its databases, or add it to databases in server.yaml.")
		}
		if _, ok := d.Logins[raw.Login]; !ok {
			return Entry{}, errors.New(at + " runs on the login " + raw.Login + ", which " + db + " does not have in server.yaml. Add " + raw.Login + " to the logins of " + db + ", or set login to a login it has, such as read.")
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

// limitsOf checks the limits of the entry at, and returns nil when the entry sets none.
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
			return nil, errors.New(at + " has limits." + c.key + " " + strconv.Itoa(*c.v) + ", and it must be 1 or more. Set it to 1 or more, or leave it out to keep the limit of server.yaml.")
		}
		*c.dst = *c.v
	}
	if raw.Timeout != nil {
		d, err := time.ParseDuration(*raw.Timeout)
		if err != nil {
			return nil, errors.New(at + " has limits.timeout " + *raw.Timeout + ", which is not a duration. Write it with a unit, such as 30s.")
		}
		if d <= 0 {
			return nil, errors.New(at + " has limits.timeout " + *raw.Timeout + ", and it must be longer than zero. Set it to the statement timeout of each call, such as 30s, or leave it out to keep the timeout of server.yaml.")
		}
		l.Timeout = d
	}
	return l, nil
}
