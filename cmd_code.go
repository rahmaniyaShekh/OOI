//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"

	"ooi/internal/code"
	"ooi/internal/procctl"
	"ooi/internal/rendezvous"
)

// cmdCode manages this device's persistent join code without starting a server.
func cmdCode(args []string) error {
	fs := flag.NewFlagSet("code", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		rotate  = fs.Bool("new", false, "generate a new code, revoking access from everyone holding the old one")
		purge   = fs.Bool("purge", false, "delete this device's code so the next start behaves like a new device")
		service = fs.String("service", rendezvous.DefaultService, "rendezvous base URL, used to build the link")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `ooi code - manage this device's join code

The code belongs to this device: it is generated once and reused on every
start, so your friend can rejoin tomorrow with nothing re-sent.

FLAGS
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := procctl.Dir()
	if err != nil {
		return err
	}

	var c string
	switch {
	case *purge:
		if err := code.Purge(dir); err != nil {
			return err
		}
		fmt.Println("code purged; the next `ooi serve` will generate a fresh one")
		return nil
	case *rotate:
		c, err = code.Rotate(dir)
		if err != nil {
			return err
		}
		fmt.Println("new code generated; the old one no longer works")
	default:
		c, _, err = code.LoadOrCreate(dir)
		if err != nil {
			return err
		}
	}

	rz := rendezvous.New(*service)
	fmt.Println()
	fmt.Printf("  code   %s\n", code.Pretty(c))
	fmt.Printf("  link   %s\n", rz.JoinURL(code.Pretty(c)))
	fmt.Println()
	fmt.Println("  Send the link. It opens the page with the code already filled in.")
	fmt.Println("  The code travels in the URL fragment, which is never sent to any server.")
	return nil
}
