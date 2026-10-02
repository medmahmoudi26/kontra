package main

// kontra workspace seed|watch|list|use|create — named workspaces under one parent folder.
//
// The Compose cluster bind-mounts KONTRA_WORKSPACES (./workspaces beside kontra/ and
// kontra-console/ by default). Each child directory is a workspace. .current in the parent names
// the active one. Discovery, watch, serve and start use only that child. Switching never remounts.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

const (
	currentFile    = ".current"
	defaultChild   = "hello"
	workspaceUsage = "usage: kontra workspace path|seed|watch|list|use|create"
)

// THE SAME RULE THE CONTROL PLANE ENFORCES, and it has to be, because this one does not get the
// last word. `control/orchestrator/src/workspaces.ts` derives a workspace's three addresses — S3
// bucket, DuckLake catalog, Temporal namespace — from this name, and its `NAME_RE` is
// `^[a-z0-9][a-z0-9-]{0,58}[a-z0-9]$`: no underscore, no dot, no uppercase.
//
// That strictness is load-bearing rather than fussy. `catalogDbName` turns hyphens into
// underscores because an unquoted Postgres identifier cannot hold a hyphen, and that substitution
// is INJECTIVE ONLY BECAUSE UNDERSCORES ARE FORBIDDEN — if both were legal, `a-b` and `a_b` would
// land on one database and two workspaces would share a catalog, which is the isolation failure
// that module exists to prevent.
//
// THIS REGEX USED TO BE `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`, which accepted all three characters
// the control plane refuses. The failure that bought this comment: `kontra workspace use
// demo_workspace` SUCCEEDED, every subsequent command read the workspace fine, and then the first
// Dataset write of the first Run died inside an Actor activity with
//
//	workspace name "demo_workspace": use letters, digits, . _ - (1–64 chars, start alnum)
//
// — a message that names `_` as legal while rejecting it, raised two layers below the command that
// made the bad choice. A name you can select and cannot write to is worse than one that is refused
// at the point of selection, so this refuses it there.
var workspaceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,58}[a-z0-9]$`)

func cmdWorkspace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", workspaceUsage)
	}
	switch args[0] {
	case "path":
		return cmdWorkspacePath(args[1:])
	case "seed":
		return cmdWorkspaceSeed(args[1:])
	case "watch":
		return cmdWorkspaceWatch(args[1:])
	case "list":
		return cmdWorkspaceList(args[1:])
	case "use":
		return cmdWorkspaceUse(args[1:])
	case "create":
		return cmdWorkspaceCreate(args[1:])
	default:
		return fmt.Errorf("%s", workspaceUsage)
	}
}

// cmdWorkspacePath prints where code goes — the one question every other command's error message
// ends up pointing at now that the workspace IS the registration.
//
// It prints a PATH AND NOTHING ELSE on success, because it is meant to be used in a shell:
//
//	mv ./myactor "$(kontra workspace path)/actors/"
func cmdWorkspacePath(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: kontra workspace path")
	}
	root := activeWorkspace()
	if root == "" {
		// ASK THE CONTROL PLANE, because it is the one that decides.
		//
		// The env vars are set for the SERVER — on a compose install they live in `.env`, which the
		// shell running this command has never read — so a CLI that answered only from its own
		// environment said "no workspace is configured" about an installation that has one and is
		// serving from it. The control plane is the authority on what it serves; this asks it, and
		// the two cannot disagree.
		if remote, err := workspaceFromAPI(); err == nil && remote != "" {
			fmt.Fprintln(cliio.Stdout, remote)
			return nil
		}
		return fmt.Errorf(
			"no workspace is configured.\n" +
				"Set KONTRA_WORKSPACES to a directory the control plane can see — on a compose\n" +
				"install that means a bind mount, so the path is the same inside and out — and\n" +
				"`kontra workspace create <name>` makes one inside it.")
	}
	fmt.Fprintln(cliio.Stdout, root)
	return nil
}

// workspaceFromAPI reads the active workspace path from the orchestrator. Empty when it has none.
func workspaceFromAPI() (string, error) {
	var got struct {
		CurrentPath string `json:"currentPath"`
	}
	if err := newAPI(orchestratorURL()).getJSON("/api/workspaces", &got); err != nil {
		return "", err
	}
	return strings.TrimSpace(got.CurrentPath), nil
}

// activeWorkspace is the code root: the named child when there is a parent, else the single tree.
// Peer of control/orchestrator/src/workspaces.ts:workspaceRoot — the two must agree, because one
// decides what the CLI prints and the other decides what the control plane serves.
func activeWorkspace() string {
	if parent := workspacesParent(); parent != "" {
		name, err := readCurrentName(parent)
		if err != nil || strings.TrimSpace(name) == "" {
			return ""
		}
		name = strings.TrimSpace(name)
		child := filepath.Join(parent, name)
		if st, err := os.Stat(child); err == nil && st.IsDir() {
			return child
		}
		return ""
	}
	return strings.TrimSpace(os.Getenv("KONTRA_WORKSPACE"))
}

// workspacesParent is the bind-mounted folder that holds named workspace children.
func workspacesParent() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_WORKSPACES")); v != "" {
		return filepath.Clean(v)
	}
	if wd, err := os.Getwd(); err == nil {
		// Sibling of the kontra checkout when the env is unset (host CLI).
		return filepath.Clean(filepath.Join(wd, "..", "workspaces"))
	}
	return filepath.Join("..", "workspaces")
}

// legacyWorkspaceRoot is the pre-named-workspaces single tree (actors/ + workflows/ at top).
func legacyWorkspaceRoot() string {
	return strings.TrimSpace(os.Getenv("KONTRA_WORKSPACE"))
}

// currentWorkspaceDir is the active child under the parent (actors/ + workflows/ live here).
func currentWorkspaceDir() (string, error) {
	if legacy := legacyWorkspaceRoot(); legacy != "" && strings.TrimSpace(os.Getenv("KONTRA_WORKSPACES")) == "" {
		return legacy, nil
	}
	parent := workspacesParent()
	name, err := readCurrentName(parent)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("no current workspace under %s — run `kontra workspace seed` or `kontra workspace create`", parent)
	}
	return filepath.Join(parent, name), nil
}

func readCurrentName(parent string) (string, error) {
	body, err := os.ReadFile(filepath.Join(parent, currentFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func writeCurrentName(parent, name string) error {
	if err := validateWorkspaceName(name); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(parent, currentFile), []byte(name+"\n"), 0o644)
}

func validateWorkspaceName(name string) error {
	if !workspaceNameRe.MatchString(name) {
		// THE MESSAGE NAMES THE RULE THAT IS ACTUALLY ENFORCED. It used to read "use letters,
		// digits, . _ -", which told a reader holding `demo_workspace` that their name was fine.
		return fmt.Errorf("workspace name %q: use lowercase letters, digits and dashes "+
			"(2–60 chars, start and end alphanumeric). No underscore, dot or uppercase: the name "+
			"becomes an S3 bucket, a Postgres catalog and a Temporal namespace, and the "+
			"hyphen-to-underscore mapping those need is only unambiguous while underscores are "+
			"illegal here", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("workspace name %q is reserved", name)
	}
	return nil
}

func listWorkspaceNames(parent string) ([]string, error) {
	ents, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

func parentHasNamedChildren(parent string) (bool, error) {
	names, err := listWorkspaceNames(parent)
	return len(names) > 0, err
}

func workspaceIsEmpty(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	for _, e := range ents {
		name := e.Name()
		if name == "." || name == ".." || strings.HasPrefix(name, ".") {
			continue
		}
		return false, nil
	}
	return true, nil
}

func seedRoot() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_SEED_DIR")); v != "" {
		return v
	}
	return "/opt/kontra/seed"
}

func seedInto(dir string) error {
	src := seedRoot()
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("seed templates not found at %s (set KONTRA_SEED_DIR): %w", src, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	empty, err := workspaceIsEmpty(dir)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}
	// Stage inside the workspace so rename stays on one device. The parent of a
	// bind-mounted workspace is the container overlay; rename from there is EXDEV.
	tmp, err := os.MkdirTemp(dir, ".seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(src, tmp); err != nil {
		return err
	}
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// cmdWorkspaceSeed ensures the parent has a hello child (when empty), seeds it, and sets .current.
func cmdWorkspaceSeed(args []string) error {
	fs := flag.NewFlagSet("workspace seed", flag.ContinueOnError)
	defaultDir := workspacesParent()
	if legacy := legacyWorkspaceRoot(); legacy != "" && strings.TrimSpace(os.Getenv("KONTRA_WORKSPACES")) == "" {
		defaultDir = legacy
	}
	dirFlag := fs.String("dir", defaultDir, "workspaces parent (or legacy single workspace) directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Legacy: --dir (or KONTRA_WORKSPACE) is the code root itself, not a parent of named children.
	if strings.TrimSpace(os.Getenv("KONTRA_WORKSPACES")) == "" && *dirFlag == legacyWorkspaceRoot() && legacyWorkspaceRoot() != "" {
		if err := os.MkdirAll(*dirFlag, 0o755); err != nil {
			return err
		}
		empty, err := workspaceIsEmpty(*dirFlag)
		if err != nil {
			return err
		}
		if !empty {
			fmt.Fprintln(cliio.Stdout, "workspace seed: not empty, left alone")
			return nil
		}
		if err := seedInto(*dirFlag); err != nil {
			return err
		}
		fmt.Fprintf(cliio.Stdout, "workspace seed: copied starter actor and workflow into %s\n", *dirFlag)
		return nil
	}
	parent := dirFlag
	if err := os.MkdirAll(*parent, 0o755); err != nil {
		return err
	}
	has, err := parentHasNamedChildren(*parent)
	if err != nil {
		return err
	}
	current, err := readCurrentName(*parent)
	if err != nil {
		return err
	}

	if !has {
		child := filepath.Join(*parent, defaultChild)
		if err := seedInto(child); err != nil {
			return err
		}
		if err := writeCurrentName(*parent, defaultChild); err != nil {
			return err
		}
		fmt.Fprintf(cliio.Stdout, "workspace seed: created %s and set it current\n", child)
		return nil
	}

	if current == "" {
		names, err := listWorkspaceNames(*parent)
		if err != nil {
			return err
		}
		pick := names[0]
		for _, n := range names {
			if n == defaultChild {
				pick = defaultChild
				break
			}
		}
		if err := writeCurrentName(*parent, pick); err != nil {
			return err
		}
		fmt.Fprintf(cliio.Stdout, "workspace seed: set current to %s\n", pick)
		return nil
	}

	child := filepath.Join(*parent, current)
	empty, err := workspaceIsEmpty(child)
	if err != nil {
		return err
	}
	if empty {
		if err := seedInto(child); err != nil {
			return err
		}
		fmt.Fprintf(cliio.Stdout, "workspace seed: copied starter into %s\n", child)
		return nil
	}
	fmt.Fprintf(cliio.Stdout, "workspace seed: %s already has workspaces (current=%s), left alone\n", *parent, current)
	return nil
}

func cmdWorkspaceList(args []string) error {
	fs := flag.NewFlagSet("workspace list", flag.ContinueOnError)
	parent := fs.String("dir", workspacesParent(), "workspaces parent directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	names, err := listWorkspaceNames(*parent)
	if err != nil {
		return err
	}
	current, err := readCurrentName(*parent)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintf(cliio.Stdout, "no workspaces under %s\n", *parent)
		return nil
	}
	for _, n := range names {
		mark := " "
		if n == current {
			mark = "*"
		}
		fmt.Fprintf(cliio.Stdout, "%s %s\n", mark, n)
	}
	return nil
}

func cmdWorkspaceUse(args []string) error {
	fs := flag.NewFlagSet("workspace use", flag.ContinueOnError)
	parent := fs.String("dir", workspacesParent(), "workspaces parent directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kontra workspace use <name>")
	}
	name := fs.Arg(0)
	if err := validateWorkspaceName(name); err != nil {
		return err
	}
	child := filepath.Join(*parent, name)
	st, err := os.Stat(child)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("workspace %q does not exist under %s", name, *parent)
	}
	if err := writeCurrentName(*parent, name); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "workspace use: current is %s\n", name)
	return nil
}

func cmdWorkspaceCreate(args []string) error {
	fs := flag.NewFlagSet("workspace create", flag.ContinueOnError)
	parent := fs.String("dir", workspacesParent(), "workspaces parent directory")
	seed := fs.Bool("seed", false, "copy the starter actor and workflow into the new workspace")
	use := fs.Bool("use", true, "make the new workspace current")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kontra workspace create <name> [--seed] [--use=false]")
	}
	name := fs.Arg(0)
	if err := validateWorkspaceName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(*parent, 0o755); err != nil {
		return err
	}
	child := filepath.Join(*parent, name)
	if err := os.Mkdir(child, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("workspace %q already exists", name)
		}
		return err
	}
	if *seed {
		if err := seedInto(child); err != nil {
			return err
		}
	}
	if *use {
		if err := writeCurrentName(*parent, name); err != nil {
			return err
		}
	}
	fmt.Fprintf(cliio.Stdout, "workspace create: %s", child)
	if *seed {
		fmt.Fprint(cliio.Stdout, " (seeded)")
	}
	if *use {
		fmt.Fprint(cliio.Stdout, " (current)")
	}
	fmt.Fprintln(cliio.Stdout)
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func cmdWorkspaceWatch(args []string) error {
	fs := flag.NewFlagSet("workspace watch", flag.ContinueOnError)
	parent := fs.String("dir", workspacesParent(), "workspaces parent directory")
	api := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "workspace watch: parent %s → %s\n", *parent, *api)
	seen := map[string]string{}
	var watching string
	for {
		root, err := resolveWatchRoot(*parent)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workspace watch: %v\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if root != watching {
			watching = root
			clear(seen)
			fmt.Fprintf(cliio.Stdout, "workspace watch: current → %s\n", root)
		}
		if err := reconcileWorkspace(root, *api, seen); err != nil {
			fmt.Fprintf(os.Stderr, "workspace watch: %v\n", err)
		}
		time.Sleep(3 * time.Second)
	}
}

func resolveWatchRoot(parent string) (string, error) {
	name, err := readCurrentName(parent)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("no .current under %s", parent)
	}
	child := filepath.Join(parent, name)
	st, err := os.Stat(child)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("current workspace %q missing under %s", name, parent)
	}
	return child, nil
}

func reconcileWorkspace(root, api string, seen map[string]string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		base := d.Name()
		kind := ""
		switch base {
		case "actor.json":
			kind = "actor"
		case "workflow.json":
			kind = "workflow"
		default:
			return nil
		}
		dir := filepath.Dir(path)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		stamp := fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
		if seen[dir] == stamp {
			return nil
		}
		if err := postSource(api, kind, dir); err != nil {
			fmt.Fprintf(os.Stderr, "workspace watch: %s %s: %v\n", kind, dir, err)
			return nil
		}
		if kind == "actor" {
			cmd := exec.Command("kontra", "deploy", "--actor", dir, "--registry", registryForWatch(), "--override")
			var buf bytes.Buffer
			cmd.Stdout = io.MultiWriter(os.Stderr, &buf)
			cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
			if err := cmd.Run(); err != nil {
				if !watchDeploySettled(buf.String(), err) {
					fmt.Fprintf(os.Stderr, "workspace watch: deploy %s: %v\n", dir, err)
					return nil
				}
			}
		}
		seen[dir] = stamp
		fmt.Fprintf(cliio.Stdout, "workspace watch: registered %s %s\n", kind, dir)
		return nil
	})
}

// watchDeploySettled is whether this watch turn should stop retrying a deploy.
// "already deployed" is the steady state after the first push; retrying it every
// three seconds re-registers the Nexus endpoint and matching never dispatches.
func watchDeploySettled(output string, err error) bool {
	return err == nil || strings.Contains(output, "already deployed")
}

func registryForWatch() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_REGISTRY")); v != "" {
		return v
	}
	return "127.0.0.1:5000"
}

func postSource(api, kind, dir string) error {
	body, err := json.Marshal(map[string]string{"path": dir})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(api, "/")+"/api/sources/"+kind, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	if tok := strings.TrimSpace(os.Getenv("KONTRA_RUN_TOKEN")); tok != "" {
		req.Header.Set("authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 410 IS THE SERVER SAYING THERE IS NOTHING TO DO, NOT A FAILURE.
	//
	// ADR 0049 made the workspace itself the registration, and `routes/sources.ts` answers this
	// POST with 410 Gone on purpose — "the workspace is the registration. Put the folder in the
	// workspace." The watcher kept posting anyway, so every scan of every folder printed
	//
	//	workspace watch: actor …/actors/subfinder: 410 Gone: {"error":"registering a folder is gone…
	//
	// to stderr, forever, for seven actors. Nothing was broken: the folders ARE served, because
	// being in the workspace is what serves them. The only thing the call produced was noise, and
	// noise in the one stream an operator watches for real failures is worse than no call at all.
	//
	// Swallowed rather than deleted, so a control plane older than ADR 0049 still gets registered
	// by a newer CLI.
	if resp.StatusCode == http.StatusGone {
		return nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
