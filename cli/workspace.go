package main

// kontra workspace seed|watch|list|use|create — named workspaces under one parent folder.
//
// The Compose cluster bind-mounts KONTRA_WORKSPACES (./workspaces.kontra under the compose
// directory by default). Each child directory is a workspace. .current in the parent names the
// active one. Discovery, watch, serve and start use only that child. Switching never remounts.

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
	currentFile     = ".current"
	defaultChild    = "hello"
	workspaceUsage  = "usage: kontra workspace seed|watch|list|use|create"
)

var workspaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func cmdWorkspace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", workspaceUsage)
	}
	switch args[0] {
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

// workspacesParent is the bind-mounted folder that holds named workspace children.
func workspacesParent() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_WORKSPACES")); v != "" {
		return v
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "workspaces.kontra")
	}
	return "workspaces.kontra"
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
		return fmt.Errorf("workspace name %q: use letters, digits, . _ - (1–64 chars, start alnum)", name)
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
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
