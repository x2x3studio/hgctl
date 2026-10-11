package hgctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVectorCleanupBackportIsExactAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqlite_vec_index.py")
	original := "before\n" + legacyVectorCleanupSQL + "\nafter\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := repairVectorCleanupFile(path)
	if err != nil || !changed {
		t.Fatalf("changed=%v error=%v", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "before\n" + fixedVectorCleanupSQL + "\nafter\n"
	if string(got) != want {
		t.Fatal("backport changed unrelated source")
	}
	backup, err := os.ReadFile(path + ".hgctl-before-vector-cleanup")
	if err != nil || string(backup) != original {
		t.Fatal("original source was not preserved")
	}
	info, _ := os.Stat(path)
	changed, err = repairVectorCleanupFile(path)
	if err != nil || changed {
		t.Fatalf("second changed=%v error=%v", changed, err)
	}
	after, _ := os.Stat(path)
	if !info.ModTime().Equal(after.ModTime()) {
		t.Fatal("second repair rewrote source")
	}
}

func TestVectorCleanupBackportLeavesUnknownVersionsUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqlite_vec_index.py")
	original := "different upstream implementation"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := repairVectorCleanupFile(path)
	if err != nil || changed {
		t.Fatalf("changed=%v error=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Fatal("unknown source changed")
	}
}

func TestVectorCleanupBackportRefusesAmbiguityAndConflictingBackup(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "sqlite_vec_index.py")
		original := legacyVectorCleanupSQL
		if duplicate {
			original = strings.Repeat(original+"\n", 2)
		}
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		if !duplicate {
			if err := os.WriteFile(path+".hgctl-before-vector-cleanup", []byte("different version"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if changed, err := repairVectorCleanupFile(path); err == nil || changed {
			t.Fatal("unsafe repair accepted")
		}
		got, _ := os.ReadFile(path)
		if string(got) != original {
			t.Fatal("source changed after refusal")
		}
	}
}
