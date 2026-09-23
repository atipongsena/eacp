// Command fakeerp is a fake ERP used as the Slice A demo target.
//
// Phase 1 serves health probes only, so the network topology can be tested.
// Phase 6 adds purchase orders, operation-key lookup, worker-only credential
// checks and the failure modes in MASTER_PLAN §80.
package main

import (
	"eacp/internal/config"
	"eacp/internal/service"
)

func main() {
	service.Main("fakeerp", config.Options{RequireDatabase: false, DefaultHTTPAddr: ":8090"}, nil)
}
