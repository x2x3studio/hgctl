package product

import (
	"strings"
	"testing"
)

const sourceID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseNoteAcceptsClosedFrontmatter(t *testing.T) {
	content := "---\ntitle: The queue remains endpoint-owned\ncreated: 2026-07-21\nupdated: 2026-07-21\nsources:\n  - sha256:" + sourceID + "\n---\n\nReasoning.\n"
	note, err := ParseNote([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if note.Title != "The queue remains endpoint-owned" || len(note.Sources) != 1 || note.Sources[0] != sourceID {
		t.Fatalf("unexpected note: %#v", note)
	}
}

func TestParseNoteRejectsAmbiguousOrExtendedFrontmatter(t *testing.T) {
	tests := []string{
		"title: true",
		"title: unsafe: value",
		"created: 2026-02-31",
		"updated: 2026-07-20",
		"tags: memory",
		"  - sha256:short",
	}
	base := "---\ntitle: Safe assertion\ncreated: 2026-07-21\nupdated: 2026-07-21\nsources:\n  - sha256:" + sourceID + "\n---\n"
	for _, mutation := range tests {
		t.Run(strings.ReplaceAll(mutation, " ", "_"), func(t *testing.T) {
			content := base
			switch {
			case strings.HasPrefix(mutation, "title:"):
				content = strings.Replace(content, "title: Safe assertion", mutation, 1)
			case strings.HasPrefix(mutation, "created:"):
				content = strings.Replace(content, "created: 2026-07-21", mutation, 1)
			case strings.HasPrefix(mutation, "updated:"):
				content = strings.Replace(content, "updated: 2026-07-21", mutation, 1)
			case strings.HasPrefix(mutation, "tags:"):
				content = strings.Replace(content, "sources:", mutation+"\nsources:", 1)
			default:
				content = strings.Replace(content, "  - sha256:"+sourceID, mutation, 1)
			}
			if _, err := ParseNote([]byte(content)); err == nil {
				t.Fatalf("accepted invalid note:\n%s", content)
			}
		})
	}
}

func TestValidateCanvasChecksReferencesAndMemoryFiles(t *testing.T) {
	valid := `{"nodes":[{"id":"home","type":"file","file":"Home.md","x":0,"y":0,"width":300,"height":200},{"id":"note","type":"file","file":"memory/system/queue.md","x":400,"y":0,"width":300,"height":200}],"edges":[{"id":"edge","fromNode":"home","toNode":"note","toEnd":"arrow"}]}`
	if err := ValidateCanvas([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{"Home.md": true, "memory/system/queue.md": true}
	if err := ValidateCanvasReferences([]byte(valid), func(name string) bool { return files[name] }); err != nil {
		t.Fatal(err)
	}
	delete(files, "memory/system/queue.md")
	if err := ValidateCanvasReferences([]byte(valid), func(name string) bool { return files[name] }); err == nil {
		t.Fatal("accepted a Canvas reference to a missing Memory file")
	}
	for _, invalid := range []string{
		strings.Replace(valid, `"toNode":"note"`, `"toNode":"missing"`, 1),
		strings.Replace(valid, `memory/system/queue.md`, `../AGENTS.md`, 1),
		strings.Replace(valid, `"id":"note"`, `"id":"home"`, 1),
		`{"nodes":null,"edges":[]}`,
	} {
		if err := ValidateCanvas([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid Canvas: %s", invalid)
		}
	}
}

func TestMemoryPathsRejectTraversalAndInstructionSurfaces(t *testing.T) {
	for _, valid := range []string{
		"memory/system/queue.md", "memory/people/alice.md", "memory/2026/q3.md",
		"memory/" + strings.Repeat("a", maxMemoryPathComponentBytes-len(".md")) + ".md",
	} {
		if !IsMemoryPath(valid) {
			t.Fatalf("rejected valid path %q", valid)
		}
	}
	for _, invalid := range []string{
		"memory/../AGENTS.md", "memory/.codex/config.md", "memory/topic/CLAUDE.md",
		"memory/topic.txt", "Memory/topic.md", "memory//topic.md", "memory/topic\n.md",
		"memory/people/Alice.md", "memory/people/alice smith.md", "memory/people/caf\u00e9.md",
		"memory/people/cafe\u0301.md",
		"memory/" + strings.Repeat("a", maxMemoryPathComponentBytes-len(".md")+1) + ".md",
	} {
		if IsMemoryPath(invalid) {
			t.Fatalf("accepted invalid path %q", invalid)
		}
	}
}
