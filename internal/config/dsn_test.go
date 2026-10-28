package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestDSNNeverPrints(t *testing.T) {
	const password = "pw-7f3a-never-print"
	v := "postgres://sales:" + password + "@db.example.internal/sales_eu"
	d := DSN{value: &v}
	if d.Reveal() != v {
		t.Fatalf("Reveal returned %q, want the connection string", d.Reveal())
	}
	// held reaches the DSN through an unexported field, where fmt cannot call its methods.
	held := struct{ d DSN }{d}
	login := Login{Env: "SALES_EU_DSN", DSN: d}
	cfg := Config{Databases: map[string]Database{"sales_eu": {Logins: map[string]Login{"read": login}}}}

	var out bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
		for _, x := range []any{d, &d, held, login, cfg, &cfg} {
			fmt.Fprintf(&out, verb+"\n", x)
		}
	}
	fmt.Fprintln(&out, d.String(), d.GoString())
	raw, _ := json.Marshal(cfg)
	out.Write(raw)
	for _, h := range []slog.Handler{slog.NewTextHandler(&out, nil), slog.NewJSONHandler(&out, nil)} {
		slog.New(h).Info("config", "dsn", d, "login", login, "config", cfg)
	}
	if strings.Contains(out.String(), password) {
		t.Errorf("a printed DSN shows the password:\n%s", out.String())
	}
	if !strings.Contains(out.String(), hiddenDSN) {
		t.Errorf("no print shows the placeholder:\n%s", out.String())
	}
}
