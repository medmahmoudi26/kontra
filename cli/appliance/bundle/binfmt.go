// binfmt.go — asking a compiled file which machine it is for (ADR 0031 §2, issue 17).
//
// THE CHECK THAT REPLACES `node --version` ON A CROSS BUILD, and the reason four platforms are a
// riskier change than they look. A native build proves its runtime by running it — one exec, and a
// Node for the wrong architecture becomes a build failure instead of an `exec format error` hours
// later inside whatever hydrated it. A build for darwin/arm64 running on linux/amd64 cannot do
// that for ANY of the four binaries it assembles, and issue 13's comment says so plainly: a
// cross-platform build "must not pretend to".
//
// What it can do instead is read the headers. Every artifact in the bundle states its own
// architecture in its first twenty bytes, and a bundle whose files disagree with its manifest's
// `platform` field is exactly the failure four platforms introduce:
//
//	pnpm resolving the HOST's tree               a darwin bundle full of linux .so files
//	a prune keeping the wrong Rust triple        Temporal dies at the first NativeConnection
//	a Node tarball member from the wrong set     the entrypoint will not exec at all
//
// None of those fail the build without this file. All three archive cleanly, verify cleanly
// against their own manifest — every digest in it is correct, because the manifest describes the
// wrong bytes accurately — and fail on the user's machine.
//
// debug/elf, debug/macho AND debug/pe, NOT A HAND-ROLLED MAGIC-NUMBER TABLE. Two reasons, and the
// second one is the one that would have cost an afternoon. First, they are the same parsers the Go
// toolchain trusts. Second, `libduckdb.dylib` is a UNIVERSAL binary — two Mach-O images, x86_64
// and arm64, behind a `cafebabe` fat header — so "read the cputype at offset 4" answers x86_64 for
// a file that is equally correct for darwin/arm64. {@link binaryTargets} returns a LIST for that
// reason, and macOS is why.
//
// pe IS HERE TO PRODUCE A GOOD MESSAGE, not to support Windows. `@temporalio/core-bridge` ships an
// `x86_64-pc-windows-msvc/index.node`, and if a prune ever left it behind, "index.node is a
// Windows binary" is a sentence somebody can act on where "unrecognised file format" is not.
package bundle

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// binaryTargets reports every platform a compiled artifact can be loaded on.
//
// More than one only for a macOS universal binary. Empty is never returned without an error: a
// file this cannot read is a file the bundle cannot make a claim about, and silently passing it
// would put back the hole this exists to close.
func binaryTargets(path string) ([]Platform, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// ELF IS READ AS LINUX, and that is an assumption worth naming rather than hiding. ELF is also
	// FreeBSD's and Solaris's format, and its OSABI byte is `SYSV` on essentially every Linux
	// binary ever shipped — so the byte cannot distinguish them and nothing else in the header
	// tries. The appliance ships two operating systems; ELF means the one that is not macOS.
	if ef, err := elf.NewFile(f); err == nil {
		defer ef.Close()
		arch, ok := map[elf.Machine]string{
			elf.EM_X86_64:  "amd64",
			elf.EM_AARCH64: "arm64",
		}[ef.Machine]
		if !ok {
			return nil, fmt.Errorf("%s is an ELF binary for %s, which the appliance does not ship", path, ef.Machine)
		}
		return []Platform{{OS: "linux", Arch: arch}}, nil
	}

	// The fat header FIRST: a universal binary's leading bytes are not a Mach-O header, and
	// macho.NewFile would reject it as garbage rather than as the container it is.
	if ff, err := macho.NewFatFile(f); err == nil {
		defer ff.Close()
		var out []Platform
		for _, a := range ff.Arches {
			arch, ok := machoArch(a.Cpu)
			if !ok {
				continue // a slice for something we do not ship is not a reason to reject the file
			}
			out = append(out, Platform{OS: "darwin", Arch: arch})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s is a universal binary with no slice for any platform the appliance ships", path)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
		return out, nil
	}

	if mf, err := macho.NewFile(f); err == nil {
		defer mf.Close()
		arch, ok := machoArch(mf.Cpu)
		if !ok {
			return nil, fmt.Errorf("%s is a Mach-O binary for %s, which the appliance does not ship", path, mf.Cpu)
		}
		return []Platform{{OS: "darwin", Arch: arch}}, nil
	}

	if pf, err := pe.NewFile(f); err == nil {
		defer pf.Close()
		return nil, fmt.Errorf("%s is a Windows binary; the appliance ships linux and macOS only", path)
	}

	return nil, fmt.Errorf("%s is not an ELF, Mach-O or PE binary, so nothing can be said about which machine it runs on.\n"+
		"  A file with a native suffix that is not a native artifact is either a new kind of dependency or a truncated download", path)
}

func machoArch(c macho.Cpu) (string, bool) {
	switch c {
	case macho.CpuAmd64:
		return "amd64", true
	case macho.CpuArm64:
		return "arm64", true
	default:
		return "", false
	}
}

// verifyStagedBinaries refuses a staged tree whose compiled files are not all for the platform the
// manifest is about to claim.
//
// IT RUNS OVER THE STAGED TREE, NOT THE MANIFEST, and the difference is the whole point.
// {@link collectNativeAddons} walks for the same suffixes, so checking its output would prove only
// that two walks of the same tree agree with each other. This walks the bytes.
//
// EVERY MISMATCH, NOT THE FIRST. A wrong `--os` flag makes every platform package in the tree
// wrong at once, and reporting one file out of four sends somebody to look at that package.
func verifyStagedBinaries(stage string, target Platform) error {
	var wrong []string
	var checked int

	err := filepath.WalkDir(stage, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(stage, path)
		if relErr != nil {
			return relErr
		}
		// The runtime is checked BY NAME, because `node/bin/node` has no suffix at all and it is
		// the one file in the bundle whose being wrong makes every other check moot: nothing else
		// gets a chance to fail if the entrypoint cannot exec.
		if !hasAnySuffix(d.Name(), nativeSuffixes) && filepath.ToSlash(rel) != "node/bin/node" {
			return nil
		}
		checked++
		targets, err := binaryTargets(path)
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("  %s\n    %v", filepath.ToSlash(rel), err))
			return nil
		}
		for _, t := range targets {
			if t == target {
				return nil
			}
		}
		wrong = append(wrong, fmt.Sprintf("  %-70s is for %s", filepath.ToSlash(rel), platformList(targets)))
		return nil
	})
	if err != nil {
		return err
	}
	if checked == 0 {
		return fmt.Errorf("the staged tree contains no compiled artifacts at all — not even node/bin/node.\n" +
			"  A bundle with no native code is not a bundle of this orchestrator; something staged nothing")
	}
	if len(wrong) > 0 {
		return fmt.Errorf("this bundle says %s, and %d of its %d compiled files are not:\n%s\n"+
			"  Nothing here would fail on the building machine — a bundle for the wrong platform archives cleanly,\n"+
			"  verifies cleanly against its own manifest, and fails at the first exec on somebody else's machine",
			target, len(wrong), checked, strings.Join(wrong, "\n"))
	}
	return nil
}

func platformList(in []Platform) string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.String())
	}
	return strings.Join(out, " and ")
}
