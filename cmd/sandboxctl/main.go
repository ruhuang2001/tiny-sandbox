// Command sandboxctl is the developer CLI for tiny-sandbox. This scaffold
// implements the version and help commands; the rest of the surface in
// docs/cli.md arrives with the SDK.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = `sandboxctl — developer CLI for tiny-sandbox

usage:
  sandboxctl <command> [flags]

commands:
  version   print the build version
  help      print this message

Configuration:
  SANDBOX_DOMAIN   server base URL (default http://localhost:8080)
  SANDBOX_API_KEY  API key, when the server requires one
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "sandboxctl: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
