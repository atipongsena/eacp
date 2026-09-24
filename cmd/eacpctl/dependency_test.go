package main

import (
	"strings"
	"testing"
)

func TestDependencyBlastRadiusCommand(t *testing.T) {
	env, got := recordingAPI(t)
	for _, tc := range []struct {
		args []string
		uri  string
	}{
		{[]string{"dependency", "blast-radius", "mcp", "bb6d6030-b932-4d10-b401-8154108f91db"},
			"/v1/dependencies/blast-radius?id=bb6d6030-b932-4d10-b401-8154108f91db&kind=mcp"},
		{[]string{"dependency", "blast-radius", "system", "sap-production"},
			"/v1/dependencies/blast-radius?kind=system&name=sap-production"},
	} {
		*got = nil
		out, err := runWith(t, env, tc.args...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != "GET" || (*got)[0].uri != tc.uri {
			t.Fatalf("%v sent %v", tc.args, *got)
		}
	}
}
