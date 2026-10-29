package config_test

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// examples is the examples folder, seen from this package.
var examples = filepath.Join("..", "..", "examples")

// anyDSN returns a connection string for every dsn_env, since Load only checks that one is set.
func anyDSN(string) string { return "postgres://reader@db.example.internal:5432/helpdesk" }

// deployment holds the parts of examples/kubernetes/deployment.yaml that the test compares with server.yaml.
type deployment struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name string `yaml:"name"`
					} `yaml:"env"`
				} `yaml:"containers"`
				Volumes []struct {
					Projected struct {
						Sources []struct {
							ConfigMap struct {
								Items []struct {
									Path string `yaml:"path"`
								} `yaml:"items"`
							} `yaml:"configMap"`
						} `yaml:"sources"`
					} `yaml:"projected"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// kustomization holds the files that examples/kubernetes/kustomization.yaml turns into ConfigMaps.
type kustomization struct {
	ConfigMapGenerator []struct {
		Files []string `yaml:"files"`
	} `yaml:"configMapGenerator"`
}

// compose holds the environment of each service in examples/compose/docker-compose.yaml.
type compose struct {
	Services map[string]struct {
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

// readYAML decodes the YAML file at path into out.
func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}
}

// dsnEnvs returns every dsn_env that cfg names, sorted.
func dsnEnvs(cfg *config.Config) []string {
	set := map[string]bool{}
	for _, db := range cfg.Databases {
		for _, l := range db.Logins {
			set[l.Env] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// loadCatalog loads the catalog folder dir against the databases of cfg and returns its tool names.
func loadCatalog(t *testing.T, dir string, cfg *config.Config) []string {
	t.Helper()
	entries, reports, err := catalog.Load(dir, cfg.Databases, nil)
	if err != nil {
		t.Fatalf("the catalog %s does not load: %v", dir, err)
	}
	if len(reports) > 0 {
		t.Errorf("the catalog %s has statements that are not tools: %v", dir, reports)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// TestKubernetesExampleLoads loads the server.yaml, access.yaml and catalog
// of examples/kubernetes, and checks that the manifests deliver each
// connection string and each catalog file where server.yaml expects them.
func TestKubernetesExampleLoads(t *testing.T) {
	dir := filepath.Join(examples, "kubernetes")
	cfg, err := config.Load(filepath.Join(dir, "server.yaml"), anyDSN)
	if err != nil {
		t.Fatalf("the Kubernetes example does not load: %v", err)
	}
	if cfg.TLS == nil || cfg.Listen != "0.0.0.0:8443" || len(cfg.Callers) != 2 {
		t.Errorf("the Kubernetes example has tls %v, listen %s and %d callers, want tls, 0.0.0.0:8443 and 2", cfg.TLS != nil, cfg.Listen, len(cfg.Callers))
	}
	if names := loadCatalog(t, cfg.Catalog, cfg); !slices.Equal(names, []string{"revenue_by_region"}) {
		t.Errorf("the Kubernetes catalog holds %v, want revenue_by_region", names)
	}

	var d deployment
	readYAML(t, filepath.Join(dir, "deployment.yaml"), &d)
	var env, mounted []string
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			env = append(env, e.Name)
		}
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		for _, s := range v.Projected.Sources {
			for _, it := range s.ConfigMap.Items {
				mounted = append(mounted, it.Path)
			}
		}
	}
	for _, name := range dsnEnvs(cfg) {
		if !slices.Contains(env, name) {
			t.Errorf("server.yaml names dsn_env %s, and deployment.yaml does not set it", name)
		}
	}

	var k kustomization
	readYAML(t, filepath.Join(dir, "kustomization.yaml"), &k)
	var generated []string
	for _, g := range k.ConfigMapGenerator {
		generated = append(generated, g.Files...)
	}
	for _, f := range []string{"server.yaml", "access.yaml"} {
		if !slices.Contains(generated, f) {
			t.Errorf("kustomization.yaml does not put %s in a ConfigMap", f)
		}
	}
	err = filepath.WalkDir(filepath.Join(dir, "catalog"), func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !slices.Contains(generated, rel) {
			t.Errorf("kustomization.yaml does not put %s in a ConfigMap", rel)
		}
		if !slices.Contains(mounted, rel) {
			t.Errorf("deployment.yaml does not mount %s at that path", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cannot walk the Kubernetes catalog: %v", err)
	}
}

// TestComposeExampleLoads loads the server.yaml and access.yaml of
// examples/compose with examples/catalog, which docker-compose.yaml mounts as
// its catalog, and checks that the compose file sets each connection string.
func TestComposeExampleLoads(t *testing.T) {
	dir := filepath.Join(examples, "compose")
	cfg, err := config.Load(filepath.Join(dir, "server.yaml"), anyDSN)
	if err != nil {
		t.Fatalf("the compose example does not load: %v", err)
	}
	if len(cfg.Callers) != 1 {
		t.Fatalf("the compose example has %d callers, want 1", len(cfg.Callers))
	}
	names := loadCatalog(t, filepath.Join(examples, "catalog"), cfg)
	for _, name := range names {
		if !slices.Contains(cfg.Callers[0].Tools, name) {
			t.Errorf("the compose caller does not list the catalog tool %s", name)
		}
	}

	var c compose
	readYAML(t, filepath.Join(dir, "docker-compose.yaml"), &c)
	env := c.Services["adaptive-pg-mcp"].Environment
	for _, name := range dsnEnvs(cfg) {
		if env[name] == "" {
			t.Errorf("server.yaml names dsn_env %s, and docker-compose.yaml does not set it", name)
		}
	}
}
