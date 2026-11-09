// Command odt is the host-side helper for tunneld: `odt doctor` re-reads
// .env, applies it with docker compose and checks every moving part.
package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(helpText(""))
		return
	}
	switch args[0] {
	case "doctor":
		os.Exit(doctor(args[1:]))
	case "help", "-h", "--help":
		topic := ""
		if len(args) > 1 {
			topic = args[1]
		}
		fmt.Print(helpText(topic))
	default:
		fmt.Fprintf(os.Stderr, "odt: unknown command %q\n\n", args[0])
		fmt.Fprint(os.Stderr, helpText(""))
		os.Exit(2)
	}
}
