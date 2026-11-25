package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillsBatchCommandWithSpecialPath(t *testing.T) {
	if _, err := exec.LookPath("skills"); err != nil {
		t.Skip("skills CLI is not installed")
	}

	dir := filepath.Join(t.TempDir(), "skills %TEMP% & example!")

	skillDir := filepath.Join(dir, "skills", "visible")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: visible\ndescription: Example.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t.Context(), "skills", "--version")
	if err != nil || strings.TrimSpace(out.stdout) == "" {
		t.Fatalf("version output %q, error %v", out.stdout, err)
	}

	names, err := (githubTreeLister{}).listDirectory(t.Context(), dir, false)
	if err != nil || strings.Join(names, ",") != "visible" {
		t.Fatalf("names %v, error %v", names, err)
	}
}
