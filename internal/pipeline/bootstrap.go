package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/x2x3studio/hgctl/internal/product"
)

const (
	bootstrapGitOutputLimit = 8 * 1024 * 1024
	sharedRef               = "refs/heads/shared"
)

type BootstrapOptions struct {
	Checkout   string
	ControlSHA string
}

type BootstrapResult struct {
	Created bool
}

type bootstrapCanvas struct {
	Nodes []bootstrapNode `json:"nodes"`
	Edges []bootstrapEdge `json:"edges"`
}

type bootstrapNode struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	File   string `json:"file,omitempty"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type bootstrapEdge struct {
	ID       string `json:"id"`
	FromNode string `json:"fromNode"`
	ToNode   string `json:"toNode"`
	FromSide string `json:"fromSide"`
	ToSide   string `json:"toSide"`
	ToEnd    string `json:"toEnd"`
}

func Bootstrap(ctx context.Context, options BootstrapOptions) (BootstrapResult, error) {
	if options.Checkout == "" {
		return BootstrapResult{}, errors.New("bootstrap checkout is required")
	}
	checkout, err := filepath.Abs(options.Checkout)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("resolve bootstrap checkout: %w", err)
	}
	checkout, err = filepath.EvalSymlinks(checkout)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("resolve bootstrap checkout links: %w", err)
	}
	repository := gitRepository{directory: checkout}
	top, err := repository.run(ctx, product.MaxPathBytes, "rev-parse", "--show-toplevel")
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("verify bootstrap checkout: %w", err)
	}
	resolvedTop, err := filepath.Abs(strings.TrimSpace(string(top)))
	if err == nil {
		resolvedTop, err = filepath.EvalSymlinks(resolvedTop)
	}
	if err != nil || filepath.Clean(resolvedTop) != filepath.Clean(checkout) {
		return BootstrapResult{}, errors.New("bootstrap checkout must be the repository root")
	}

	exists, err := remoteSharedExists(ctx, repository)
	if err != nil {
		return BootstrapResult{}, err
	}
	if exists {
		return BootstrapResult{Created: false}, nil
	}

	revision, err := repository.revision(ctx, "HEAD")
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("read trusted bootstrap revision: %w", err)
	}
	if options.ControlSHA == "" || revision.Commit != options.ControlSHA {
		return BootstrapResult{}, errors.New("bootstrap checkout does not match the trusted control revision")
	}
	status, err := repository.run(ctx, bootstrapGitOutputLimit, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("inspect bootstrap checkout: %w", err)
	}
	if len(status) != 0 {
		return BootstrapResult{}, errors.New("bootstrap checkout is not clean")
	}
	if _, err := repository.run(ctx, bootstrapGitOutputLimit, "checkout", "--orphan", "shared"); err != nil {
		return BootstrapResult{}, fmt.Errorf("create local shared branch: %w", err)
	}
	if _, err := repository.run(ctx, bootstrapGitOutputLimit, "rm", "-rf", "--ignore-unmatch", "--", "."); err != nil {
		return BootstrapResult{}, fmt.Errorf("clear control files from shared branch: %w", err)
	}

	files, err := initialSharedFiles()
	if err != nil {
		return BootstrapResult{}, err
	}
	for name, content := range files {
		if err := writeBootstrapFile(filepath.Join(checkout, name), content); err != nil {
			return BootstrapResult{}, fmt.Errorf("write initial shared file %s: %w", name, err)
		}
	}
	if err := verifyBootstrapTree(checkout, files); err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{Created: true}, nil
}

func writeBootstrapFile(name string, content []byte) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func remoteSharedExists(ctx context.Context, repository gitRepository) (bool, error) {
	output, err := repository.run(ctx, 256, "ls-remote", "--heads", "origin", sharedRef)
	if err != nil {
		return false, fmt.Errorf("inspect remote shared branch: %w", err)
	}
	if len(output) == 0 {
		return false, nil
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || !commitPattern.MatchString(fields[0]) || fields[1] != sharedRef {
		return false, errors.New("remote returned an invalid shared branch advertisement")
	}
	return true, nil
}

func initialSharedFiles() (map[string][]byte, error) {
	canvas := bootstrapCanvas{
		Nodes: []bootstrapNode{
			{ID: "agents", Type: "text", Text: "Claude Code and Codex\nagent endpoints", X: -760, Y: 0, Width: 260, Height: 100},
			{ID: "hgctl", Type: "text", Text: "hgctl\ncapture, queue push, shared pull", X: -420, Y: 0, Width: 280, Height: 100},
			{ID: "queue", Type: "text", Text: "queue/<machine-id>\nwide ingress", X: -60, Y: 0, Width: 260, Height: 100},
			{ID: "dream", Type: "text", Text: "GitHub Actions\nprepare -> Dream -> publish", X: 280, Y: 0, Width: 300, Height: 100},
			{ID: "shared", Type: "file", File: "Home.md", X: 660, Y: 0, Width: 260, Height: 100},
			{ID: "recall", Type: "text", Text: "Basic Memory\nlocal auxiliary recall", X: 280, Y: 190, Width: 300, Height: 100},
		},
		Edges: []bootstrapEdge{
			{ID: "agents-hgctl", FromNode: "agents", ToNode: "hgctl", FromSide: "right", ToSide: "left", ToEnd: "arrow"},
			{ID: "hgctl-queue", FromNode: "hgctl", ToNode: "queue", FromSide: "right", ToSide: "left", ToEnd: "arrow"},
			{ID: "queue-dream", FromNode: "queue", ToNode: "dream", FromSide: "right", ToSide: "left", ToEnd: "arrow"},
			{ID: "dream-shared", FromNode: "dream", ToNode: "shared", FromSide: "right", ToSide: "left", ToEnd: "arrow"},
			{ID: "shared-recall", FromNode: "shared", ToNode: "recall", FromSide: "bottom", ToSide: "right", ToEnd: "arrow"},
			{ID: "recall-agents", FromNode: "recall", ToNode: "agents", FromSide: "left", ToSide: "bottom", ToEnd: "arrow"},
		},
	}
	canvasContent, err := json.MarshalIndent(canvas, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode initial Canvas: %w", err)
	}
	canvasContent = append(canvasContent, '\n')
	files := map[string][]byte{
		".gitignore":       []byte(".hourglass-runtime/\n"),
		"Home.md":          []byte("# Hourglass\n\nHourglass is shared memory for AI agents. Durable knowledge appears under `memory/` after queued evidence passes Dream reconciliation.\n\nUse Basic Memory for auxiliary recall. Current dialogue and primary sources remain authoritative.\n"),
		"Hourglass.canvas": canvasContent,
	}
	if err := product.ValidateCanvasReferences(canvasContent, func(name string) bool {
		_, exists := files[name]
		return exists
	}); err != nil {
		return nil, fmt.Errorf("validate initial Canvas: %w", err)
	}
	return files, nil
}

func verifyBootstrapTree(root string, expected map[string][]byte) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read bootstrap tree: %w", err)
	}
	found := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if entry.IsDir() {
			return fmt.Errorf("bootstrap tree contains unexpected directory %s", entry.Name())
		}
		found[entry.Name()] = struct{}{}
	}
	if len(found) != len(expected) {
		return errors.New("bootstrap tree does not contain exactly the initial shared files")
	}
	for name, want := range expected {
		if _, exists := found[name]; !exists {
			return fmt.Errorf("bootstrap tree is missing %s", name)
		}
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != string(want) {
			return fmt.Errorf("bootstrap file %s changed during generation", name)
		}
	}
	return nil
}
