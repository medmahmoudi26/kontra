package bundle

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE FIXTURES ARE SYNTHESIZED, NOT COPIED FROM A REAL BINARY, and that is what makes this suite
// worth having. The whole point of binfmt.go is to answer questions about platforms the test
// machine is not — a darwin/arm64 header cannot be produced by compiling something here, and
// checking in four 30 MB binaries to test a twenty-byte header would be an odd trade. Every fixture
// below is the minimum `debug/elf`, `debug/macho` and `debug/pe` will parse, written by hand from
// the same field layouts those packages read.
//
// They were checked against the real thing. The values here — EM_AARCH64 0xB7, CPU_TYPE_ARM64
// 0x0100000C, the `cafebabe` fat container — are the ones measured off the actual artifacts in
// the orchestrator's dependency tree while this file was written, including the one that motivated
// {@link binaryTargets} returning a list: `@duckdb/node-bindings-darwin-arm64/libduckdb.dylib` is
// a UNIVERSAL binary carrying x86_64 and arm64 slices, so a bundle for darwin/amd64 and a bundle
// for darwin/arm64 legitimately contain the same file.

func elfFixture(t *testing.T, machine uint16) []byte {
	t.Helper()
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F'})
	b[4] = 2                                       // ELFCLASS64
	b[5] = 1                                       // ELFDATA2LSB
	b[6] = 1                                       // EV_CURRENT
	binary.LittleEndian.PutUint16(b[16:], 3)       // ET_DYN
	binary.LittleEndian.PutUint16(b[18:], machine) // e_machine
	binary.LittleEndian.PutUint32(b[20:], 1)       // e_version
	binary.LittleEndian.PutUint16(b[52:], 64)      // e_ehsize
	return b
}

func machoFixture(t *testing.T, cpu uint32) []byte {
	t.Helper()
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b[0:], 0xfeedfacf) // MH_MAGIC_64
	binary.LittleEndian.PutUint32(b[4:], cpu)
	binary.LittleEndian.PutUint32(b[8:], 0) // cpusubtype
	binary.LittleEndian.PutUint32(b[12:], 6)
	binary.LittleEndian.PutUint32(b[16:], 0) // ncmds
	binary.LittleEndian.PutUint32(b[20:], 0) // sizeofcmds
	return b
}

// fatFixture is a universal binary: a big-endian `cafebabe` header, then one 20-byte entry per
// slice, then the Mach-O images themselves at the offsets those entries name.
func fatFixture(t *testing.T, cpus ...uint32) []byte {
	t.Helper()
	const hdr = 8
	entries := len(cpus) * 20
	offset := uint32(hdr + entries)
	// Aligned to something plausible, because macho.NewFatFile checks that slices do not overlap
	// and reads each image at the offset it is given.
	if pad := offset % 4096; pad != 0 {
		offset += 4096 - pad
	}

	var head bytes.Buffer
	_ = binary.Write(&head, binary.BigEndian, uint32(0xcafebabe))
	_ = binary.Write(&head, binary.BigEndian, uint32(len(cpus)))

	var body bytes.Buffer
	at := offset
	for _, cpu := range cpus {
		image := machoFixture(t, cpu)
		_ = binary.Write(&head, binary.BigEndian, cpu)
		_ = binary.Write(&head, binary.BigEndian, uint32(0)) // cpusubtype
		_ = binary.Write(&head, binary.BigEndian, at)
		_ = binary.Write(&head, binary.BigEndian, uint32(len(image)))
		_ = binary.Write(&head, binary.BigEndian, uint32(12)) // align
		body.Write(image)
		at += 4096
		body.Write(make([]byte, 4096-len(image)))
	}
	out := head.Bytes()
	out = append(out, make([]byte, int(offset)-len(out))...)
	return append(out, body.Bytes()...)
}

func peFixture(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 0x200)
	copy(b, []byte{'M', 'Z'})
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80) // e_lfanew
	copy(b[0x80:], []byte{'P', 'E', 0, 0})
	binary.LittleEndian.PutUint16(b[0x84:], 0x8664) // IMAGE_FILE_MACHINE_AMD64
	binary.LittleEndian.PutUint16(b[0x86:], 0)      // NumberOfSections
	return b
}

func binFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBinaryTargetsReadsEachFormat(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body []byte
		want []string
	}{
		{"elf-amd64.so", elfFixture(t, 0x3e), []string{"linux/amd64"}},
		{"elf-arm64.so", elfFixture(t, 0xb7), []string{"linux/arm64"}},
		{"macho-amd64.node", machoFixture(t, 0x01000007), []string{"darwin/amd64"}},
		{"macho-arm64.node", machoFixture(t, 0x0100000c), []string{"darwin/arm64"}},
		// The universal case, which is not hypothetical: libduckdb.dylib is one.
		{"fat.dylib", fatFixture(t, 0x01000007, 0x0100000c), []string{"darwin/amd64", "darwin/arm64"}},
	}
	for _, c := range cases {
		got, err := binaryTargets(binFile(t, dir, c.name, c.body))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i].String() != c.want[i] {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
}

// A Windows `.node` is a file the prune is supposed to have removed. If one ever survives, the
// message has to say what it IS — "unrecognised" would send somebody looking for a corrupt
// download instead of a prune that missed.
func TestBinaryTargetsNamesAWindowsBinaryAsOne(t *testing.T) {
	_, err := binaryTargets(binFile(t, t.TempDir(), "index.node", peFixture(t)))
	if err == nil {
		t.Fatal("a Windows binary was accepted")
	}
	if !strings.Contains(err.Error(), "Windows") {
		t.Errorf("the error does not say what the file is: %v", err)
	}
}

// A `.node` that is not a binary at all is not "fine because we cannot read it". It is either a
// truncated download or a dependency that has started shipping something new, and both are things
// a bundle must not carry silently.
func TestBinaryTargetsRefusesWhatItCannotIdentify(t *testing.T) {
	_, err := binaryTargets(binFile(t, t.TempDir(), "empty.node", []byte("not a binary at all")))
	if err == nil {
		t.Fatal("an unidentifiable file was accepted")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("the error does not suggest what to look at: %v", err)
	}
}

// THE FAILURE FOUR PLATFORMS INTRODUCE, in one test: a tree resolved for the wrong architecture.
// Every digest such a bundle records is correct, and every one of them describes the wrong bytes.
func TestVerifyStagedBinariesCatchesATreeResolvedForTheWrongPlatform(t *testing.T) {
	stage := t.TempDir()
	binFile(t, stage, "node/bin/node", elfFixture(t, 0x3e))
	binFile(t, stage, "orchestrator/node_modules/@duckdb/node-bindings-linux-x64/libduckdb.so", elfFixture(t, 0x3e))
	binFile(t, stage, "orchestrator/node_modules/@swc/core-linux-x64-gnu/swc.node", elfFixture(t, 0x3e))

	if err := verifyStagedBinaries(stage, Platform{OS: "linux", Arch: "amd64"}); err != nil {
		t.Fatalf("a linux/amd64 tree was refused as linux/amd64: %v", err)
	}

	err := verifyStagedBinaries(stage, Platform{OS: "darwin", Arch: "arm64"})
	if err == nil {
		t.Fatal("a tree of linux binaries passed as darwin/arm64")
	}
	// EVERY WRONG FILE, NOT THE FIRST. A wrong `--os` makes the whole tree wrong at once, and
	// naming one file out of three sends somebody to look at that one package.
	for _, want := range []string{"node/bin/node", "libduckdb.so", "swc.node", "darwin/arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

// The runtime has no suffix to walk for, and it is the one file whose being wrong makes every
// other check moot — nothing else gets a chance to fail if the entrypoint cannot exec.
func TestVerifyStagedBinariesChecksTheRuntimeToo(t *testing.T) {
	stage := t.TempDir()
	binFile(t, stage, "node/bin/node", machoFixture(t, 0x0100000c))
	binFile(t, stage, "orchestrator/node_modules/x/y.so", elfFixture(t, 0x3e))

	err := verifyStagedBinaries(stage, Platform{OS: "linux", Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "node/bin/node") {
		t.Fatalf("a darwin runtime in a linux tree was not reported: %v", err)
	}
}

// A universal binary is correct for BOTH macOS targets, and refusing it for one of them would fail
// every darwin/amd64 build over `libduckdb.dylib`.
func TestVerifyStagedBinariesAcceptsAUniversalBinaryForEitherMac(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		stage := t.TempDir()
		binFile(t, stage, "node/bin/node", machoFixture(t, map[string]uint32{"amd64": 0x01000007, "arm64": 0x0100000c}[arch]))
		binFile(t, stage, "orchestrator/node_modules/@duckdb/node-bindings-darwin-arm64/libduckdb.dylib",
			fatFixture(t, 0x01000007, 0x0100000c))
		if err := verifyStagedBinaries(stage, Platform{OS: "darwin", Arch: arch}); err != nil {
			t.Errorf("darwin/%s refused a universal library: %v", arch, err)
		}
	}
}

// A staged tree with no compiled files is not a passing check — it is a build that staged nothing,
// and a gate that reports success on an empty tree is the gate this repo has been burned by before.
func TestVerifyStagedBinariesRefusesATreeWithNoCompiledFiles(t *testing.T) {
	stage := t.TempDir()
	binFile(t, stage, "orchestrator/dist/main.js", []byte("console.log(1)"))

	err := verifyStagedBinaries(stage, Platform{OS: "linux", Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "no compiled artifacts") {
		t.Fatalf("an empty tree passed the platform gate: %v", err)
	}
}
