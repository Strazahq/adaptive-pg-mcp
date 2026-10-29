package core

import (
	"slices"
	"testing"
	"time"

	"github.com/strazahq/adaptive-pg-mcp/internal/catalog"
	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

func TestRaisedLimitsAreReported(t *testing.T) {
	server := config.Limits{Rows: 1000, Bytes: 1048576, Timeout: 30 * time.Second, WriteRows: 100}
	raises := func(field, to, from string) Finding {
		return Finding{Text: "finance/tools.yaml: the catalog tool revenue_by_region raises limits." + field + " to " + to + ", above the " + from + " in server.yaml. Lower it if that is not intended."}
	}
	cases := []struct {
		name   string
		limits *catalog.Limits
		want   []Finding
	}{
		{"no limits of its own", nil, nil},
		{"lower and equal caps", &catalog.Limits{Rows: 10, Bytes: 1048576, Timeout: time.Second, WriteRows: 100}, nil},
		{"every cap raised", &catalog.Limits{Rows: 5000, Bytes: 4194304, Timeout: 2 * time.Minute, WriteRows: 1500},
			[]Finding{raises("rows", "5,000", "1,000"), raises("bytes", "4,194,304", "1,048,576"), raises("timeout", "2m0s", "30s"), raises("write_rows", "1,500", "100")}},
		{"one cap raised", &catalog.Limits{Rows: 5, WriteRows: 101}, []Finding{raises("write_rows", "101", "100")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := catalog.Entry{Name: "revenue_by_region", Source: "finance/revenue.sql", Limits: c.limits}
			if got := raisedLimits(server, e); !slices.Equal(got, c.want) {
				t.Errorf("raisedLimits found:\n%v\nwant:\n%v", got, c.want)
			}
		})
	}
}
