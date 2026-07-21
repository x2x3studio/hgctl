package product

import (
	"path"
	"strings"
	"unicode/utf8"
)

const (
	MaxPathBytes                = 4096
	maxMemoryPathComponentBytes = 128
)

var instructionFiles = map[string]struct{}{
	".mcp.json":          {},
	"agents.md":          {},
	"agents.override.md": {},
	"claude.local.md":    {},
	"claude.md":          {},
	"gemini.md":          {},
	"skill.md":           {},
}

var instructionDirectories = map[string]struct{}{
	".agents": {},
	".claude": {},
	".codex":  {},
	".cursor": {},
	".gemini": {},
}

func IsInstructionSurface(name string) bool {
	if !validRelativePath(name) {
		return true
	}
	parts := strings.Split(strings.ToLower(name), "/")
	if _, blocked := instructionFiles[parts[len(parts)-1]]; blocked {
		return true
	}
	for _, part := range parts {
		if _, blocked := instructionDirectories[part]; blocked {
			return true
		}
	}
	return false
}

func IsMemoryPath(name string) bool {
	if !validRelativePath(name) || IsInstructionSurface(name) {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) < 2 || parts[0] != "memory" || !strings.HasSuffix(parts[len(parts)-1], ".md") {
		return false
	}
	for _, part := range parts[1:] {
		if !validMemoryPathComponent(part) {
			return false
		}
	}
	return true
}

func IsSemanticPath(name string) bool {
	return name == "Home.md" || name == "Hourglass.canvas" || IsMemoryPath(name)
}

func validRelativePath(name string) bool {
	if name == "" || len(name) > MaxPathBytes || !utf8.ValidString(name) || strings.Contains(name, "\\") {
		return false
	}
	for _, r := range name {
		if r == 0 || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "."
}

func validMemoryPathComponent(value string) bool {
	if len(value) == 0 || len(value) > maxMemoryPathComponentBytes || !lowerLetterOrDigit(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !lowerLetterOrDigit(character) && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func lowerLetterOrDigit(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}
