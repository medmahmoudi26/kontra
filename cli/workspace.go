package main

// kontra workspace seed | watch — the Compose cluster's source of truth for local code.

import (
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
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

func cmdWorkspace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kontra workspace seed|watch")
	}
	switch args[0] {
	case "seed":
		return cmdWorkspaceSeed(args[1:])
	case "watch":
		return cmdWorkspaceWatch(args[1:])
	default:
		return fmt.Errorf("usage: kontra workspace seed|watch")
	}
}

func workspaceDir() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_WORKSPACE")); v != "" {
		return v
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "workspace")
	}
	return "workspace"
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

func cmdWorkspaceSeed(args []string) error {
	fs := flag.NewFlagSet("workspace seed", flag.ContinueOnError)
	dir := fs.String("dir", workspaceDir(), "workspace directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	empty, err := workspaceIsEmpty(*dir)
	if err != nil {
		return err
	}
	if !empty {
		fmt.Fprintln(cliio.Stdout, "workspace seed: not empty, left alone")
		return nil
	}
	src := seedRoot()
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("seed templates not found at %s (set KONTRA_SEED_DIR): %w", src, err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(*dir), ".seed-*")
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
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(*dir, e.Name())); err != nil {
			return err
		}
	}
	fmt.Fprintf(cliio.Stdout, "workspace seed: copied starter actor and workflow into %s\n", *dir)
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
	dir := fs.String("dir", workspaceDir(), "workspace directory")
	api := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "workspace watch: %s → %s\n", *dir, *api)
	seen := map[string]string{}
	for {
		if err := reconcileWorkspace(*dir, *api, seen); err != nil {
			fmt.Fprintf(os.Stderr, "workspace watch: %v\n", err)
		}
		time.Sleep(3 * time.Second)
	}
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
			cmd := exec.Command("kontra", "deploy", "--actor", dir, "--registry", registryForWatch())
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "workspace watch: deploy %s: %v\n", dir, err)
				return nil
			}
		}
		seen[dir] = stamp
		fmt.Fprintf(cliio.Stdout, "workspace watch: registered %s %s\n", kind, dir)
		return nil
	})
}

func registryForWatch() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_REGISTRY")); v != "" {
		return v
	}
	return "registry:5000"
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
