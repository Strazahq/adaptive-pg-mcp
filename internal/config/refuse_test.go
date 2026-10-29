package config

import (
	"fmt"
	"strings"
	"testing"
)

// refusal is one broken config and the sentence Load must answer with. $DIR
// stands for the folder that holds both files.
type refusal struct {
	name   string
	change func(server, access map[string]any, env map[string]string)
	want   string
}

// runRefusals loads each case and compares the full refusal sentence.
func runRefusals(t *testing.T, cases []refusal) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, a, e := serverDoc(), accessDoc(), environment()
			c.change(s, a, e)
			_, dir, err := load(t, marshal(t, s, a, e))
			want := strings.ReplaceAll(c.want, "$DIR", dir)
			if err == nil {
				t.Fatalf("Load accepted the config, want the refusal: %s", want)
			}
			if err.Error() != want {
				t.Errorf("refusal:\n got %s\nwant %s", err, want)
			}
		})
	}
}

func TestLoadNamesEveryMissingKey(t *testing.T) {
	const why = "and only tls, allow_plain_http, allow_cross_database and allow_superuser may be left out"
	const callerWhy = "and every caller needs name, token_sha256, tools and databases"
	server := func(path string) func(s, a map[string]any, e map[string]string) {
		return func(s, a map[string]any, e map[string]string) { del(s, path) }
	}
	access := func(path string) func(s, a map[string]any, e map[string]string) {
		return func(s, a map[string]any, e map[string]string) { del(a, path) }
	}
	runRefusals(t, []refusal{
		{"version", server("version"), "$DIR/server.yaml does not set version, which names the format the file is written in. Add version: 1."},
		{"mode", server("mode"), "$DIR/server.yaml does not set mode, " + why + ". Add mode: standalone, or mode: straza behind the Straza gateway."},
		{"listen", server("listen"), "$DIR/server.yaml does not set listen, " + why + ". Add listen: stdio, or an address such as 127.0.0.1:8080."},
		{"free_sql", server("free_sql"), "$DIR/server.yaml does not set free_sql, " + why + ". Add free_sql: off, read or read-write."},
		{"limits", server("limits"), "$DIR/server.yaml does not set limits, " + why + ". Add limits with rows, bytes, timeout and write_rows."},
		{"limits.rows", server("limits.rows"), "$DIR/server.yaml does not set limits.rows, " + why + ". Add limits.rows, the most rows a read answers, such as 1000."},
		{"limits.bytes", server("limits.bytes"), "$DIR/server.yaml does not set limits.bytes, " + why + ". Add limits.bytes, the most bytes a read answers, such as 1048576."},
		{"limits.timeout", server("limits.timeout"), "$DIR/server.yaml does not set limits.timeout, " + why + ". Add limits.timeout, the statement timeout of each call, such as 30s."},
		{"limits.write_rows", server("limits.write_rows"), "$DIR/server.yaml does not set limits.write_rows, " + why + ". Add limits.write_rows, the most rows a write may change, such as 100."},
		{"catalog", server("catalog"), "$DIR/server.yaml does not set catalog, " + why + ". Add catalog: the folder of the catalog, or none."},
		{"access in standalone", server("access"), "$DIR/server.yaml does not set access, which mode standalone needs to know its callers. Add access: the path to access.yaml."},
		{"databases", server("databases"), "$DIR/server.yaml does not set databases, " + why + ". Add databases with at least one database and its login."},
		{"databases empty", func(s, a map[string]any, e map[string]string) { set(s, "databases", map[string]any{}) },
			"$DIR/server.yaml does not set databases, " + why + ". Add databases with at least one database and its login."},
		{"login and logins", server("databases.sales_eu.login"),
			"$DIR/server.yaml database sales_eu sets neither login nor logins, so adaptive-pg-mcp has no login to connect with. Add login with dsn_env, or logins with read and write."},
		{"dsn_env of a login", server("databases.sales_eu.login.dsn_env"),
			"$DIR/server.yaml does not set dsn_env for the login of sales_eu, so adaptive-pg-mcp cannot find its connection string. Add dsn_env: the name of the environment variable that holds it."},
		{"read of logins", server("databases.helpdesk.logins.read"),
			"$DIR/server.yaml database helpdesk has logins without read, and logins needs both read and write. Add logins.read with dsn_env, or use login for one login that reads and writes."},
		{"write of logins", server("databases.helpdesk.logins.write"),
			"$DIR/server.yaml database helpdesk has logins without write, and logins needs both read and write. Add logins.write with dsn_env, or use login for one login that reads and writes."},
		{"dsn_env of a named login", server("databases.helpdesk.logins.finance_reader.dsn_env"),
			"$DIR/server.yaml does not set dsn_env for the finance_reader login of helpdesk, so adaptive-pg-mcp cannot find its connection string. Add dsn_env: the name of the environment variable that holds it."},
		{"access version", access("version"), "$DIR/access.yaml does not set version, which names the format the file is written in. Add version: 1."},
		{"callers", access("callers"), "$DIR/access.yaml does not set callers, so no request could pass the token check. Add callers with at least one caller."},
		{"caller name", access("callers.0.name"), "$DIR/access.yaml caller 1 does not set name, " + callerWhy + ". Add name: a name of its own for the caller."},
		{"caller token_sha256", access("callers.0.token_sha256"), "$DIR/access.yaml caller support_bot does not set token_sha256, " + callerWhy + ". Add the hash that adaptive-pg-mcp token prints."},
		{"caller tools", access("callers.1.tools"), "$DIR/access.yaml caller ops_bot does not set tools, " + callerWhy + ". Add tools: the tools it may use, such as [list_tables, read_rows]."},
		{"caller databases", access("callers.0.databases"), "$DIR/access.yaml caller support_bot does not set databases, " + callerWhy + ". Add databases: the databases it may use, such as [helpdesk]."},
	})
}

func TestLoadAppliesTheModeRules(t *testing.T) {
	straza := func(s map[string]any) {
		set(s, "mode", "straza")
		del(s, "access")
		set(s, "gateway_tokens_sha256", []any{hashA})
	}
	runRefusals(t, []refusal{
		{"gateway tokens in standalone", func(s, a map[string]any, e map[string]string) { set(s, "gateway_tokens_sha256", []any{hashA}) },
			"$DIR/server.yaml sets gateway_tokens_sha256 in mode standalone, where access.yaml names the callers. Remove gateway_tokens_sha256, or set mode: straza."},
		{"access in straza", func(s, a map[string]any, e map[string]string) { straza(s); set(s, "access", "access.yaml") },
			"$DIR/server.yaml sets access in mode straza, where Straza decides who may call. Remove access, or set mode: standalone."},
		{"no gateway tokens in straza", func(s, a map[string]any, e map[string]string) { straza(s); del(s, "gateway_tokens_sha256") },
			"$DIR/server.yaml does not set gateway_tokens_sha256, which mode straza needs to check the tokens Straza holds. Add the SHA-256 of each Straza token in lowercase hex, as adaptive-pg-mcp token prints it."},
		{"empty gateway tokens in straza", func(s, a map[string]any, e map[string]string) { straza(s); set(s, "gateway_tokens_sha256", []any{}) },
			"$DIR/server.yaml does not set gateway_tokens_sha256, which mode straza needs to check the tokens Straza holds. Add the SHA-256 of each Straza token in lowercase hex, as adaptive-pg-mcp token prints it."},
		{"gateway token in capitals", func(s, a map[string]any, e map[string]string) {
			straza(s)
			set(s, "gateway_tokens_sha256", []any{hashA, strings.ToUpper("ab" + hashA[2:])})
		}, "$DIR/server.yaml has entry 2 of gateway_tokens_sha256 that is not 64 lowercase hexadecimal characters. Write each hash as adaptive-pg-mcp token prints it."},
	})
}

func TestLoadAppliesTheTransportRules(t *testing.T) {
	pair := map[string]any{"cert_file": "server.crt", "key_file": "server.key"}
	cleartext := func(listen, loopback string) string {
		return "$DIR/server.yaml has listen " + listen + ", which is not localhost or a loopback address, so without tls the bearer tokens of its callers would cross the network in cleartext. Add tls with cert_file and key_file, set listen to a loopback address such as " + loopback + ", or set allow_plain_http: true if something in front of the server encrypts the traffic, such as a service mesh."
	}
	runRefusals(t, []refusal{
		{"tls with stdio", func(s, a map[string]any, e map[string]string) { set(s, "listen", "stdio"); set(s, "tls", pair) },
			"$DIR/server.yaml sets tls with listen stdio, where no network connection carries the calls. Remove tls, or set listen to the address to serve HTTPS on."},
		{"allow_plain_http with stdio", func(s, a map[string]any, e map[string]string) {
			set(s, "listen", "stdio")
			set(s, "allow_plain_http", true)
		},
			"$DIR/server.yaml sets allow_plain_http: true with listen stdio, where no HTTP is served. Remove allow_plain_http, or set listen to the address to serve HTTP on."},
		{"tls and allow_plain_http", func(s, a map[string]any, e map[string]string) { set(s, "tls", pair); set(s, "allow_plain_http", true) },
			"$DIR/server.yaml sets both tls and allow_plain_http: true, which contradict each other, because with tls adaptive-pg-mcp serves HTTPS only. Remove allow_plain_http to serve HTTPS, or remove tls to serve plain HTTP."},
		{"tls without cert_file", func(s, a map[string]any, e map[string]string) {
			set(s, "tls", map[string]any{"key_file": "server.key"})
		},
			"$DIR/server.yaml sets tls without cert_file, and tls needs both cert_file and key_file. Add tls.cert_file: the path to the PEM file of the server's certificate."},
		{"tls as an empty mapping", func(s, a map[string]any, e map[string]string) { set(s, "tls", map[string]any{}) },
			"$DIR/server.yaml sets tls without cert_file, and tls needs both cert_file and key_file. Add tls.cert_file: the path to the PEM file of the server's certificate."},
		{"tls without key_file", func(s, a map[string]any, e map[string]string) {
			set(s, "tls", map[string]any{"cert_file": "server.crt"})
		},
			"$DIR/server.yaml sets tls without key_file, and tls needs both cert_file and key_file. Add tls.key_file: the path to the PEM file of the certificate's private key."},
		{"every IPv4 interface", func(s, a map[string]any, e map[string]string) { set(s, "listen", "0.0.0.0:8080") },
			cleartext("0.0.0.0:8080", "127.0.0.1:8080")},
		{"every IPv6 interface", func(s, a map[string]any, e map[string]string) { set(s, "listen", "[::]:9443") },
			cleartext("[::]:9443", "127.0.0.1:9443")},
		{"an address of another network", func(s, a map[string]any, e map[string]string) { set(s, "listen", "192.0.2.10:8080") },
			cleartext("192.0.2.10:8080", "127.0.0.1:8080")},
		{"a host name other than localhost", func(s, a map[string]any, e map[string]string) { set(s, "listen", "mcp.example.internal:8080") },
			cleartext("mcp.example.internal:8080", "127.0.0.1:8080")},
		{"allow_plain_http false", func(s, a map[string]any, e map[string]string) {
			set(s, "listen", "0.0.0.0:8080")
			set(s, "allow_plain_http", false)
		},
			cleartext("0.0.0.0:8080", "127.0.0.1:8080")},
	})
}

func TestLoadRefusesBadValues(t *testing.T) {
	runRefusals(t, []refusal{
		{"mode", func(s, a map[string]any, e map[string]string) { set(s, "mode", "gateway") },
			"$DIR/server.yaml has mode gateway, which is neither standalone nor straza. Set mode to standalone, or to straza behind the Straza gateway."},
		{"listen", func(s, a map[string]any, e map[string]string) { set(s, "listen", "localhost") },
			"$DIR/server.yaml has listen localhost, which is neither stdio nor a host and port. Set listen to stdio, or to an address such as 127.0.0.1:8080."},
		{"listen with a port and no host", func(s, a map[string]any, e map[string]string) { set(s, "listen", ":8080") },
			"$DIR/server.yaml has listen :8080, which names a port and no host, so it would serve on every network interface. Set listen to 127.0.0.1:8080 to serve this machine only, or name the address to serve on."},
		{"listen with a named port", func(s, a map[string]any, e map[string]string) { set(s, "listen", "127.0.0.1:http") },
			"$DIR/server.yaml has listen 127.0.0.1:http, which is neither stdio nor a host and port. Set listen to stdio, or to an address such as 127.0.0.1:8080."},
		{"free_sql", func(s, a map[string]any, e map[string]string) { set(s, "free_sql", "write") },
			"$DIR/server.yaml has free_sql write, which is not off, read or read-write. Set free_sql to one of them."},
		{"rows zero", func(s, a map[string]any, e map[string]string) { set(s, "limits.rows", 0) },
			"$DIR/server.yaml has limits.rows 0, and it must be 1 or more. Set limits.rows to the most rows a read answers, such as 1000."},
		{"write_rows negative", func(s, a map[string]any, e map[string]string) { set(s, "limits.write_rows", -5) },
			"$DIR/server.yaml has limits.write_rows -5, and it must be 1 or more. Set limits.write_rows to the most rows a write may change, such as 100."},
		{"timeout without a unit", func(s, a map[string]any, e map[string]string) { set(s, "limits.timeout", 30) },
			"$DIR/server.yaml has limits.timeout 30, which is not a duration. Write it with a unit, such as 30s."},
		{"timeout zero", func(s, a map[string]any, e map[string]string) { set(s, "limits.timeout", "0s") },
			"$DIR/server.yaml has limits.timeout 0s, and it must be longer than zero. Set it to the statement timeout of each call, such as 30s."},
		{"login and logins both", func(s, a map[string]any, e map[string]string) {
			set(s, "databases.helpdesk.login", map[string]any{"dsn_env": "HELPDESK_DSN"})
		}, "$DIR/server.yaml database helpdesk sets both login and logins. Keep login for one login that reads and writes, or logins with read and write."},
		{"a connection string in dsn_env", func(s, a map[string]any, e map[string]string) {
			set(s, "databases.sales_eu.login.dsn_env", "postgres://sales:hunter2@db.example.internal/sales_eu")
		}, "$DIR/server.yaml has a dsn_env for the login of sales_eu that is not an environment variable name. Put the connection string in an environment variable, and set dsn_env to the variable's name, such as SALES_EU_DSN."},
		{"version 2", func(s, a map[string]any, e map[string]string) { set(s, "version", 2) },
			"$DIR/server.yaml has version 2, and this build reads version 1 only. Write the file in version 1, or run a build that reads version 2."},
		{"access version 0", func(s, a map[string]any, e map[string]string) { set(a, "version", 0) },
			"$DIR/access.yaml has version 0, and this build reads version 1 only. Write the file in version 1, or run a build that reads version 0."},
	})
}

func TestLoadNamesTheUnsetVariable(t *testing.T) {
	runRefusals(t, []refusal{
		{"single login", func(s, a map[string]any, e map[string]string) { delete(e, "SALES_EU_DSN") },
			"the environment variable SALES_EU_DSN, which server.yaml names for the login of sales_eu, is not set or is empty, so adaptive-pg-mcp cannot connect to sales_eu. Set it to the connection string of that login."},
		{"named login", func(s, a map[string]any, e map[string]string) { delete(e, "HELPDESK_WRITE_DSN") },
			"the environment variable HELPDESK_WRITE_DSN, which server.yaml names for the write login of helpdesk, is not set or is empty, so adaptive-pg-mcp cannot connect to helpdesk. Set it to the connection string of that login."},
		{"empty", func(s, a map[string]any, e map[string]string) { e["HELPDESK_READ_DSN"] = "" },
			"the environment variable HELPDESK_READ_DSN, which server.yaml names for the read login of helpdesk, is not set or is empty, so adaptive-pg-mcp cannot connect to helpdesk. Set it to the connection string of that login."},
	})
}

func TestLoadAppliesTheAccessRules(t *testing.T) {
	caller := func(a map[string]any, i int) map[string]any { return a["callers"].([]any)[i].(map[string]any) }
	runRefusals(t, []refusal{
		{"caller twice", func(s, a map[string]any, e map[string]string) { caller(a, 1)["name"] = "support_bot" },
			"$DIR/access.yaml lists the caller support_bot twice. Give each caller its own name."},
		{"hash twice", func(s, a map[string]any, e map[string]string) { caller(a, 1)["token_sha256"] = hashA },
			"$DIR/access.yaml gives the callers support_bot and ops_bot the same token_sha256, so the token check could not tell them apart. Give each caller its own token."},
		{"hash too short", func(s, a map[string]any, e map[string]string) { caller(a, 0)["token_sha256"] = hashA[:63] },
			"$DIR/access.yaml caller support_bot has a token_sha256 that is not 64 lowercase hexadecimal characters. Write the hash as adaptive-pg-mcp token prints it."},
		{"unknown database", func(s, a map[string]any, e map[string]string) {
			caller(a, 1)["databases"] = []any{"sales_eu", "billing"}
		},
			"$DIR/access.yaml caller ops_bot names the database billing, which server.yaml does not define. Remove billing from the caller, or add it to databases in server.yaml."},
		{"no access file", func(s, a map[string]any, e map[string]string) { set(s, "access", "missing.yaml") },
			"adaptive-pg-mcp cannot read access.yaml at $DIR/missing.yaml, which server.yaml names in access: open $DIR/missing.yaml: no such file or directory. Create the file, or correct the path in access."},
	})
}

func TestLoadNamesAnUnknownKey(t *testing.T) {
	cases := []struct {
		name, file, path, key string
	}{
		{"top level", "server.yaml", "lmits", "lmits"},
		{"in limits", "server.yaml", "limits.row", "row"},
		{"in a database", "server.yaml", "databases.sales_eu.logn", "logn"},
		{"in a login", "server.yaml", "databases.helpdesk.logins.read.dsn", "dsn"},
		{"in access.yaml", "access.yaml", "callers.1.tool", "tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, a := serverDoc(), accessDoc()
			doc := s
			if c.file == "access.yaml" {
				doc = a
			}
			set(doc, c.path, "x")
			f := marshal(t, s, a, environment())
			text := f.server
			if c.file == "access.yaml" {
				text = f.access
			}
			_, dir, err := load(t, f)
			want := fmt.Sprintf("%s/%s line %d has the key %s, which this build does not read. Remove it, or correct its spelling.", dir, c.file, lineOf(t, text, c.key+": x"), c.key)
			if err == nil || err.Error() != want {
				t.Errorf("refusal:\n got %v\nwant %s", err, want)
			}
		})
	}
}

func TestLoadRefusesFilesThatAreNotConfig(t *testing.T) {
	cases := []struct {
		name, server, want string
	}{
		{"empty", "", "$DIR/server.yaml does not set version, which names the format the file is written in. Add version: 1."},
		{"bad syntax", "version: 1\nmode: standalone\n  listen: stdio\n", "$DIR/server.yaml is not valid YAML: yaml: line 3: mapping values are not allowed in this context. Correct the syntax on the line it names."},
		{"a list", "- version: 1\n", "$DIR/server.yaml line 1 has a value of the wrong type, where a mapping of keys belongs. Write a mapping of keys there."},
		{"a key twice", "version: 1\nmode: standalone\nmode: straza\n", "$DIR/server.yaml line 3 sets the key mode a second time in one place. Keep one of the two."},
		{"version as text", "version: one\n", "$DIR/server.yaml line 1 has a value of the wrong type, where a whole number belongs. Write a whole number there."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, dir, err := load(t, files{server: c.server, env: environment()})
			want := strings.ReplaceAll(c.want, "$DIR", dir)
			if err == nil || err.Error() != want {
				t.Errorf("refusal:\n got %v\nwant %s", err, want)
			}
		})
	}
}

func TestLoadNamesTheLineOfAWrongType(t *testing.T) {
	s := serverDoc()
	set(s, "limits.rows", "many")
	f := marshal(t, s, accessDoc(), environment())
	_, dir, err := load(t, f)
	want := fmt.Sprintf("%s/server.yaml line %d has a value of the wrong type, where a whole number belongs. Write a whole number there.", dir, lineOf(t, f.server, "rows: many"))
	if err == nil || err.Error() != want {
		t.Errorf("refusal:\n got %v\nwant %s", err, want)
	}
}

func TestLoadNamesAMissingServerFile(t *testing.T) {
	_, err := Load("/nonexistent/server.yaml", func(string) string { return "" })
	want := "adaptive-pg-mcp cannot read server.yaml at /nonexistent/server.yaml: open /nonexistent/server.yaml: no such file or directory. Pass its path with -config, or create the file."
	if err == nil || err.Error() != want {
		t.Errorf("refusal:\n got %v\nwant %s", err, want)
	}
}

func TestLoadRefusesASecondDocument(t *testing.T) {
	second := func(file string, line int) string {
		return "$DIR/" + file + " holds a second YAML document from line " + fmt.Sprint(line) + ", which adaptive-pg-mcp would ignore, because it reads one document per file. Keep one document per file."
	}
	valid := marshal(t, serverDoc(), accessDoc(), environment())
	next := strings.Count(valid.server, "\n") + 1
	cases := []struct {
		name, server, access, want string
	}{
		{"server.yaml with a second document", valid.server + "---\nmode: straza\n", valid.access, second("server.yaml", next)},
		{"server.yaml that ends with an empty document", valid.server + "---\n", valid.access, second("server.yaml", next)},
		{"access.yaml with a second document", valid.server, valid.access + "---\ncallers: []\n", second("access.yaml", strings.Count(valid.access, "\n")+1)},
		{"server.yaml that starts with a document marker loads", "---\n" + valid.server, valid.access, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, dir, err := load(t, files{server: c.server, access: c.access, env: environment()})
			want := strings.ReplaceAll(c.want, "$DIR", dir)
			switch {
			case want == "" && err != nil:
				t.Errorf("Load refused one document after a --- line: %v", err)
			case want != "" && (err == nil || err.Error() != want):
				t.Errorf("refusal:\n got %v\nwant %s", err, want)
			}
		})
	}
}
