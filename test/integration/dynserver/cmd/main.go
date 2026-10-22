// Command dynserver runs the IFA dynamic multi-scrape integration test server.
//
// This binary is compiled solely for integration testing. It is loaded into a
// kind cluster where IFA's collector scrapes it. It is not part of the IFA
// production binary and must never be shipped in the IFA container image.
//
// Usage:
//
//	dynserver [-addr :9100]
//
// The server binds to addr (default :9100) and serves all scenario endpoints.
// See the dynserver package for a full description of the available endpoints
// and scenario state machines.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/p95labs/ifa/test/integration/dynserver"
)

func main() {
	addr := flag.String("addr", ":9100", "address to listen on")
	flag.Parse()

	srv := dynserver.New()
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	log.Printf("dynserver: listening on %s", *addr)
	log.Printf("dynserver: serving synthetic integration-test metrics — not real vLLM or GPU data")
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("dynserver: %v", err)
	}
}
