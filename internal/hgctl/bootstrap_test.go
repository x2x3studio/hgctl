package hgctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharedBootstrapNoopsWhenTheBranchExists(t *testing.T) {
	checks := 0
	triggered := false
	err := ensureSharedBranchWith(context.Background(), sharedBootstrapOperations{
		branchExists: func(context.Context) (bool, error) {
			checks++
			return true, nil
		},
		trigger: func(context.Context) error {
			triggered = true
			return nil
		},
		wait: func(context.Context, time.Duration) error {
			t.Fatal("NOOP waited for a workflow")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checks != 1 || triggered {
		t.Fatalf("checks=%d triggered=%t", checks, triggered)
	}
}

func TestSharedBootstrapTriggersOnceAndWaitsForPublication(t *testing.T) {
	checks := 0
	triggers := 0
	waits := 0
	err := ensureSharedBranchWith(context.Background(), sharedBootstrapOperations{
		branchExists: func(context.Context) (bool, error) {
			checks++
			return checks == 3, nil
		},
		trigger: func(context.Context) error {
			triggers++
			return nil
		},
		wait: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checks != 3 || triggers != 1 || waits != 2 {
		t.Fatalf("checks=%d triggers=%d waits=%d", checks, triggers, waits)
	}
}

func TestSharedBootstrapHonorsCancellationWhilePolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	err := ensureSharedBranchWith(ctx, sharedBootstrapOperations{
		branchExists: func(context.Context) (bool, error) { return false, nil },
		trigger:      func(context.Context) error { return nil },
		wait: func(context.Context, time.Duration) error {
			waits++
			cancel()
			return ctx.Err()
		},
	})
	if !errors.Is(err, context.Canceled) || waits != 1 {
		t.Fatalf("err=%v waits=%d", err, waits)
	}
}

func TestTriggerSharedBootstrapUsesAuthenticatedDefaultBranchWorkflow(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gh.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HGCTL_GH_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HGCTL_GH_LOG", logPath)
	if err := triggerSharedBootstrap(context.Background(), "git@github.com:x2x3studio/hourglass.git"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "auth status --hostname github.com\nworkflow run bootstrap.yml --repo github.com/x2x3studio/hourglass\n"
	if string(content) != want {
		t.Fatalf("gh calls = %q, want %q", content, want)
	}
	if strings.Contains(string(content), "--ref") {
		t.Fatal("bootstrap selected a caller-controlled workflow ref")
	}
}

func TestTriggerSharedBootstrapRejectsUnsupportedRemote(t *testing.T) {
	if err := triggerSharedBootstrap(context.Background(), "/tmp/hourglass.git"); err == nil {
		t.Fatal("accepted a non-GitHub remote for automatic bootstrap")
	}
}
