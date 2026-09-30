//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"ooi/internal/code"
	"ooi/internal/install"
	"ooi/internal/procctl"
	"ooi/internal/rendezvous"

	"golang.org/x/sys/windows"
)

// cmdInstall copies this exe to %LOCALAPPDATA%\Programs\ooi and puts that
// folder on the user PATH, so `ooi` works from any terminal. No admin needed.
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	ghToken := fs.String("github-token", "", "save a read-only GitHub token (encrypted) so `ooi update` can reach the private repo")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `ooi install - install ooi for the current user

Copies this ooi.exe to %LOCALAPPDATA%\Programs\ooi and adds that folder to your
user PATH, so "ooi" works in any new terminal. No administrator rights needed.
Safe to run again: it replaces the installed copy with this one.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Replacing a running instance's exe is fine (it is renamed aside), but the
	// running one would keep the old code; stop it so the new one is used.
	wasRunning := stopIfRunning()

	res, err := install.Install(exe)
	if err != nil {
		return err
	}
	// A token can come from the flag, or from OOI_GITHUB_TOKEN (how install.ps1
	// passes it, so it never appears on a command line). Saved encrypted.
	tok := strings.TrimSpace(*ghToken)
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("OOI_GITHUB_TOKEN"))
	}
	tokenSaved := false
	if tok != "" {
		if dir, err := procctl.Dir(); err == nil {
			if err := install.SaveToken(dir, tok); err != nil {
				return err
			}
			tokenSaved = true
		}
	}

	fmt.Println()
	fmt.Println("  ooi installed")
	fmt.Println()
	fmt.Printf("  location   %s\n", res.Target)
	switch {
	case res.PathAdded:
		fmt.Println("  PATH       added (for your user account)")
	default:
		fmt.Println("  PATH       already set")
	}
	if tokenSaved {
		fmt.Println("  updates    GitHub token saved (encrypted) - `ooi update` needs nothing more")
	}
	// The code is created now, not on first serve, so the user can hand it to
	// their friend straight away. It is kept across updates and reinstalls.
	if dir, err := procctl.Dir(); err == nil {
		if c, _, err := code.LoadOrCreate(dir); err == nil {
			fmt.Printf("  your code  %s   (permanent for this PC)\n", code.Pretty(c))
			fmt.Printf("  link       %s\n", rendezvous.New("").JoinURL(code.Pretty(c)))
		}
	}
	fmt.Println()
	if res.PathAdded {
		fmt.Println("  Open a NEW terminal so it picks up the PATH change, then:")
	} else {
		fmt.Println("  From any terminal:")
	}
	fmt.Println()
	fmt.Println("    ooi verify          check capture protection on this PC")
	fmt.Println("    ooi start           start in the background, prints the link for your friend")
	fmt.Println("    ooi status          see the code, connection and protection state")
	fmt.Println("    ooi stop            stop it")
	fmt.Println("    ooi update          get the latest release")
	fmt.Println()
	if wasRunning {
		fmt.Println("  (a running instance was stopped; start it again with `ooi start`)")
		fmt.Println()
	}
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	purge := fs.Bool("purge", false, "also delete this device's join code and state (%LOCALAPPDATA%\\ooi)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stopIfRunning()
	dataDir, _ := procctl.Dir()
	if err := install.Uninstall(dataDir, *purge); err != nil {
		return err
	}
	fmt.Println("ooi uninstalled: removed from PATH and from", mustDir())
	if *purge {
		fmt.Println("join code and state deleted")
	} else {
		fmt.Println("your join code was kept (use `ooi uninstall --purge` to delete it)")
	}
	return nil
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	force := fs.Bool("force", false, "reinstall even if already on the latest version")
	check := fs.Bool("check", false, "only report whether an update is available")
	tokenFlag := fs.String("token", "", "GitHub token with read access to the repo (saved encrypted for next time)")
	forget := fs.Bool("forget-token", false, "delete the saved GitHub token and exit")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `ooi update - install the latest release from GitHub

The OOI repository is private, so GitHub needs a token to hand out the release.
Create one once at  https://github.com/settings/personal-access-tokens/new :
  Repository access: Only select repositories -> OOI
  Permissions:       Contents -> Read-only
It is saved encrypted for your Windows account (DPAPI) and reused.

FLAGS
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	dataDir, err := procctl.Dir()
	if err != nil {
		return err
	}
	if *forget {
		if err := install.ForgetToken(dataDir); err != nil {
			return err
		}
		fmt.Println("saved GitHub token deleted")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	token := strings.TrimSpace(*tokenFlag)
	saveIt := token != ""
	if token == "" {
		token, _ = install.Token(dataDir)
	}

	fmt.Println("checking https://github.com/" + install.Repo + " ...")
	rel, err := install.Latest(ctx, token)
	if (errors.Is(err, install.ErrPrivate) || errors.Is(err, install.ErrBadToken)) && isTerminal() {
		fmt.Println()
		fmt.Println("  " + err.Error())
		fmt.Println("  Create a token at https://github.com/settings/personal-access-tokens/new")
		fmt.Println("  (Only select repositories: OOI; Permissions: Contents = Read-only).")
		fmt.Print("  Paste it here (hidden, Enter to cancel): ")
		token = readHidden()
		fmt.Println()
		if token == "" {
			return errors.New("update cancelled")
		}
		saveIt = true
		rel, err = install.Latest(ctx, token)
	}
	if err != nil {
		return err
	}
	if saveIt {
		if err := install.SaveToken(dataDir, token); err != nil {
			return err
		}
		fmt.Println("  token saved (encrypted for this Windows user)")
	}

	current := "v" + strings.TrimPrefix(version, "v")
	fmt.Printf("  installed %s, latest %s\n", current, rel.Tag)
	if rel.Tag == current && !*force {
		fmt.Println("  already up to date")
		return nil
	}
	if *check {
		fmt.Println("  an update is available: run `ooi update`")
		return nil
	}

	fmt.Println("  downloading and verifying (SHA-256) ...")
	tmp, err := install.Download(ctx, rel, token)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	wasRunning := stopIfRunning()
	res, err := install.Install(tmp)
	if err != nil {
		return err
	}
	fmt.Printf("  updated to %s at %s\n", rel.Tag, res.Target)
	if wasRunning {
		fmt.Println("  the running instance was stopped; start it again with `ooi start`")
	}
	return nil
}

// isTerminal reports whether stdin is an interactive console.
func isTerminal() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}

// readHidden reads one line from the console without echoing it.
func readHidden() string {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT)
		defer windows.SetConsoleMode(h, mode)
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

// stopIfRunning asks a running instance to exit and reports whether one was.
func stopIfRunning() bool {
	dir, err := procctl.Dir()
	if err != nil {
		return false
	}
	st, err := procctl.Load(dir)
	if err != nil {
		return false
	}
	if cl, err := controlClientFor(st.Local); err == nil && cl.Stop() == nil && waitGone(st.PID, 5*time.Second) {
		procctl.Clear(dir)
		return true
	}
	procctl.Kill(st.PID)
	waitGone(st.PID, 3*time.Second)
	procctl.Clear(dir)
	return true
}

func mustDir() string {
	d, _ := install.Dir()
	return d
}

// ownsConsole reports whether this process is the only one attached to its
// console, which is the case when ooi.exe was double-clicked in Explorer rather
// than run from a terminal.
func ownsConsole() bool {
	k := syscall.NewLazyDLL("kernel32.dll")
	p := k.NewProc("GetConsoleProcessList")
	var ids [4]uint32
	n, _, _ := p.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}

// doubleClicked handles ooi.exe being opened from Explorer: install it, explain
// how to use it from a terminal, and keep the window open to read.
func doubleClicked() {
	exe, _ := os.Executable()
	if install.IsInstalledCopy(exe) {
		fmt.Println("ooi is already installed. It is a terminal program:")
		fmt.Println()
		fmt.Println("  open Windows Terminal or PowerShell and run   ooi start")
		fmt.Println()
		usage(os.Stdout)
	} else if err := cmdInstall(nil); err != nil {
		fmt.Println("install failed:", err)
	}
	fmt.Print("Press Enter to close this window...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
