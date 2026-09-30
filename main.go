//go:build windows

// Command ooi shows a friend's browser-shared screen in a window that Windows
// excludes from every software screen-capture path, over the internet.
//
// The friend opens a web page and types (or clicks) a short code; the two
// connect directly, peer to peer, with WebRTC. Media never touches a server.
// The overlay is protected with WDA_EXCLUDEFROMCAPTURE, always on, and stays
// protected across reconnects and while it is toggled off screen.
//
// Run "ooi help" for usage.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"ooi/internal/install"
	"ooi/internal/winapi"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Every coordinate in this program is a physical pixel, which is only true
	// if the process declares per-monitor DPI awareness before it asks Windows
	// anything, so it happens first.
	winapi.EnablePerMonitorDPI()

	if len(os.Args) < 2 {
		// Double-clicked in Explorer: install and explain, instead of flashing
		// a usage message in a console that closes immediately.
		if ownsConsole() {
			doubleClicked()
			return
		}
		usage(os.Stdout)
		os.Exit(2)
	}
	// Remove the previous exe an update left behind, now that it has exited.
	install.CleanupOld()

	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "code":
		err = cmdCode(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
	case "uninstall":
		err = cmdUninstall(os.Args[2:])
	case "update":
		err = cmdUpdate(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "probe-touchpad":
		err = cmdProbeTouchpad(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "stop":
		err = cmdStop(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("ooi", version)
		fmt.Println(decoderVersion())
		if exe, e := os.Executable(); e == nil {
			where := exe
			if !install.IsInstalledCopy(exe) {
				where += "   (not installed - run `ooi install`)"
			}
			fmt.Println("  running from: " + where)
		}
	case "help", "--help", "-h":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "ooi: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}

	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ooi:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `ooi - capture-protected remote screen overlay, over the internet

USAGE
  ooi <command> [flags]

COMMANDS
  serve     Start the receiver: overlay window + WebRTC, print the join code
  code      Print, show as a link, or rotate this device's join code
  verify    Measure capture protection against every Windows capture API
  demo      Leave a protected window up so you can try recording it yourself
  status    Show the running instance
  stop      Stop the running instance
  install   Install for this user and add to PATH (no admin needed)
  update    Download and install the latest release from GitHub
  uninstall Remove ooi (keeps your join code unless --purge)
  version   Print the version and the linked decoder libraries

QUICK START
  ooi install                once: put ooi on PATH (open a new terminal after)
  ooi verify                 confirm capture protection works on this PC
  ooi serve                  start; print the code to send your friend
  ooi serve --detach         same, but return to the prompt immediately
  ooi stop                   shut the detached instance down

Your friend opens the printed link (or the site and types the code), picks a
window or screen to share, and it appears in your protected overlay.

Run "ooi serve -h" for the full flag list.
`)
}
