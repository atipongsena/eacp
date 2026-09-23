package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"eacp/integrations/governance/microsoftagt"
)

type names []string

func (n *names) String() string     { return fmt.Sprint(*n) }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

// runPDPDevCerts writes a development-only mutual-TLS PKI for the AGT
// sidecar (ADR-002 §8). It keeps an existing complete PKI.
func runPDPDevCerts(args []string, getenv func(string) string, out io.Writer) error {
	if env := getenv("EACP_ENV"); env != "development" && env != "test" {
		return errors.New("pdp-dev-certs refused: requires EACP_ENV=development or test; production issues certificates from its own CA")
	}
	fs := flag.NewFlagSet("pdp-dev-certs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory for ca.pem, server*.pem and client*.pem")
	var server names
	fs.Var(&server, "name", "DNS name or IP address of the sidecar (repeatable)")
	if err := fs.Parse(args); err != nil || *dir == "" || len(server) == 0 || fs.NArg() != 0 {
		return errors.New("usage: eacpctl pdp-dev-certs --dir <dir> --name <host> [--name <host>...]")
	}
	if err := microsoftagt.WriteDevPKI(*dir, server); err != nil {
		return err
	}
	fmt.Fprintf(out, "development PDP PKI ready in %s\n", *dir)
	return nil
}
