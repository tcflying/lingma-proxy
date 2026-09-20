// Command genicon writes the Windows resource object that carries the desktop
// app icon.
//
// The Go linker picks up any *.syso in the package directory, so a manual
// `go build -tags production` needs this file to exist or the exe gets the
// generic Windows icon. `wails build` generates its own copy at build time and
// removes it afterwards, which is why the output must never be committed: two
// .rsrc sections fail the link with "too many .rsrc sections".
//
// The group is registered under ID 3 because that is what Wails looks up at
// runtime (winc.AppIconID); an icon embedded under any other name is present in
// the exe but invisible to the window and therefore to the taskbar.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tc-hib/winres"
)

func main() {
	iconPath := flag.String("ico", "build/windows/icon.ico", "icon to embed")
	outPath := flag.String("out", "lpicon.syso", "resource object to write")
	arch := flag.String("arch", "amd64", "target architecture")
	flag.Parse()

	if err := run(*iconPath, *outPath, *arch); err != nil {
		fmt.Fprintln(os.Stderr, "genicon:", err)
		os.Exit(1)
	}
}

func run(iconPath, outPath, arch string) error {
	f, err := os.Open(iconPath)
	if err != nil {
		return err
	}
	defer f.Close()

	ico, err := winres.LoadICO(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", iconPath, err)
	}

	rs := winres.ResourceSet{}
	// winres.RT_ICON is ID 3, which SetIcon uses as the group identifier.
	if err := rs.SetIcon(winres.RT_ICON, ico); err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := rs.WriteObject(out, winres.Arch(arch)); err != nil {
		return err
	}
	fmt.Printf("%s: icon group at ID 3 -> %s\n", iconPath, outPath)
	return nil
}
