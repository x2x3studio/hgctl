package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReadSharedTreeValidatesTheProduct(t *testing.T) {
	repository := newTestRepository(t)
	writeTestFile(t, repository, ".gitignore", ".hourglass-runtime/\n")
	writeTestFile(t, repository, "Home.md", "# Hourglass\n")
	writeTestFile(t, repository, "Hourglass.canvas", `{"nodes":[],"edges":[]}`)
	writeSeenLedger(t, repository, map[string]string{testDigest: testMachine})
	writeTestFile(t, repository, "memory/system/queue.md", "---\ntitle: Queue branches remain endpoint-owned\ncreated: 2026-07-21\nupdated: 2026-07-21\nsources:\n  - sha256:"+testDigest+"\n---\n")
	commitTestRepository(t, repository)

	git := gitRepository{directory: repository}
	revision, err := git.revision(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	entries, contents, _, err := readSharedTree(context.Background(), git, revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 || len(contents) != 5 {
		t.Fatalf("unexpected shared tree size: %d entries, %d contents", len(entries), len(contents))
	}
}

func TestReadSharedTreeRejectsInvalidProductSurfaces(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, string){
		"instruction": func(t *testing.T, repository string) {
			writeTestFile(t, repository, "memory/topic/CLAUDE.md", "instruction\n")
		},
		"symlink": func(t *testing.T, repository string) {
			if err := os.MkdirAll(filepath.Join(repository, "memory"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../Home.md", filepath.Join(repository, "memory", "link.md")); err != nil {
				t.Fatal(err)
			}
		},
		"missing Canvas file": func(t *testing.T, repository string) {
			writeTestFile(t, repository, "Hourglass.canvas", `{"nodes":[{"id":"missing","type":"file","file":"memory/missing.md","x":0,"y":0,"width":300,"height":200}],"edges":[]}`)
		},
		"legacy seen receipt": func(t *testing.T, repository string) {
			writeTestFile(t, repository, ".hourglass/seen/"+testDigest[:2]+"/"+testDigest, testMachine+"\n")
		},
		"legacy rejection receipt": func(t *testing.T, repository string) {
			writeTestFile(t, repository, ".hourglass/rejected/"+testMachine+"/"+testCommit+".json", "{}\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository := newTestRepository(t)
			writeTestFile(t, repository, ".gitignore", ".hourglass-runtime/\n")
			writeTestFile(t, repository, "Home.md", "# Hourglass\n")
			writeTestFile(t, repository, "Hourglass.canvas", `{"nodes":[],"edges":[]}`)
			setup(t, repository)
			commitTestRepository(t, repository)
			git := gitRepository{directory: repository}
			revision, err := git.revision(context.Background(), "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSharedTree(context.Background(), git, revision); err == nil {
				t.Fatal("accepted a forbidden shared tree")
			}
		})
	}
}

func newTestRepository(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	runTestGit(t, directory, "init", "--quiet")
	runTestGit(t, directory, "config", "user.name", "test")
	runTestGit(t, directory, "config", "user.email", "test@example.com")
	return directory
}

func writeTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitTestRepository(t *testing.T, directory string) {
	t.Helper()
	runTestGit(t, directory, "add", "--all")
	runTestGit(t, directory, "commit", "--quiet", "-m", "fixture")
}

func runTestGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}

func seenLedgerFiles(t *testing.T, entries map[string]string) map[string][]byte {
	t.Helper()
	shards := make(map[string]map[string]string)
	for id, machine := range entries {
		shard := id[:2]
		if shards[shard] == nil {
			shards[shard] = make(map[string]string)
		}
		shards[shard][id] = machine
	}
	files := make(map[string][]byte, len(shards))
	for shard, values := range shards {
		content, err := encodeSeenShard(shard, values)
		if err != nil {
			t.Fatal(err)
		}
		files[".hourglass/seen/"+shard+".json"] = content
	}
	return files
}

func writeSeenLedger(t *testing.T, root string, entries map[string]string) {
	t.Helper()
	for name, content := range seenLedgerFiles(t, entries) {
		writeTestFile(t, root, name, string(content))
	}
}
