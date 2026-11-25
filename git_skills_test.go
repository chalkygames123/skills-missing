package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOfflineFallbackWithSkillsCLI(t *testing.T) {
	if _, err := exec.LookPath("skills"); err != nil {
		t.Skip("skills CLI is not installed")
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	revision := strings.Repeat("a", 40)

	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	runner := func(_ context.Context, name string, args ...string) (output, error) {
		if name == "skills" {
			switch strings.Join(args, " ") {
			case "--version":
				return output{stdout: "1.9.0\n"}, nil
			case "list --json", "list --json -g":
				return output{stdout: "[]"}, nil
			}
		}

		if name == "git" {
			switch {
			case slices.Contains(args, "ls-remote"):
				return output{stdout: revision + "\tHEAD\n"}, nil
			case slices.Contains(args, "clone"), slices.Contains(args, "fetch"):
				return output{}, nil
			case slices.Contains(args, "ls-tree"):
				return output{stdout: "100644 blob x\tskills/visible/SKILL.md\x00100644 blob y\t.github/skills/hidden/SKILL.md\x00"}, nil
			case slices.Contains(args, "show"):
				return output{stdout: "---\nname: visible\ndescription: Visible.\n---\n"}, nil
			}
		}

		return output{}, fmt.Errorf("unexpected command %s %v", name, args)
	}

	var stdout, warnings bytes.Buffer

	a := app{run: runner, cacheDir: t.TempDir(), warn: &warnings}
	lister := githubTreeLister{client: server.Client(), apiBaseURL: server.URL, runGit: runner, warn: a.commandWarning}
	a.listRemoteSkills = lister.listWithFallback

	for range 2 {
		stdout.Reset()

		if err := a.execute(t.Context(), []string{"example/repo"}, &stdout); err != nil {
			t.Fatal(err)
		}

		if got, want := stdout.String(), "example/repo\tvisible\n"; got != want {
			t.Fatalf("output %q, want %q", got, want)
		}
	}

	if got := requests.Load(); got != 1 {
		t.Fatalf("API requests %d, want 1 (second run should use cache)", got)
	}

	if got := warnings.String(); !strings.Contains(got, "API rate limit remaining: 0") || !strings.Contains(got, "trying Git") {
		t.Fatalf("missing fallback diagnostic: %q", got)
	}
}

func TestGitSkillReaderIncludesVisibleSubmoduleSkills(t *testing.T) {
	rootRevision := strings.Repeat("a", 40)
	moduleRevision := strings.Repeat("b", 40)
	repositories := make(map[string]string)
	runner := func(_ context.Context, name string, args ...string) (output, error) {
		if name != "git" {
			return output{}, fmt.Errorf("unexpected command %s", name)
		}

		if i := slices.Index(args, "clone"); i >= 0 {
			repositories[args[len(args)-1]] = args[len(args)-2]
			return output{}, nil
		}

		i := slices.Index(args, "-C")
		if i < 0 {
			return output{}, fmt.Errorf("missing repository in %v", args)
		}

		remote := repositories[args[i+1]]
		switch args[i+2] {
		case "fetch":
			return output{}, nil
		case "ls-tree":
			if strings.Contains(remote, "github.com") {
				return output{stdout: "100644 blob x\t.gitmodules\x00100644 blob y\t.github/skills/hidden/SKILL.md\x00160000 commit " + moduleRevision + "\tvendor/module\x00"}, nil
			}

			return output{stdout: "100644 blob x\tskills/visible/SKILL.md\x00100644 blob y\t.hidden/skills/secret/SKILL.md\x00"}, nil
		case "show":
			if strings.HasSuffix(args[i+3], ":.gitmodules") {
				return output{stdout: "[submodule \"module\"]\n path = vendor/module\n url = https://example.org/module.git\n"}, nil
			}

			return output{stdout: "---\nname: visible\ndescription: Visible.\n---\n"}, nil
		}

		return output{}, fmt.Errorf("unexpected command %v", args)
	}

	reader := gitSkillReader{run: runner, root: t.TempDir(), files: make(map[string][]byte)}
	if err := reader.read(t.Context(), "https://github.com/example/repo.git", rootRevision, "", map[string]bool{}, 0); err != nil {
		t.Fatal(err)
	}

	if len(reader.files) != 1 || reader.files["vendor/module/skills/visible/SKILL.md"] == nil {
		t.Fatalf("unexpected projected files: %v", reader.files)
	}
}

func TestGitSkillReaderReportsSubmoduleFailure(t *testing.T) {
	revision := strings.Repeat("a", 40)
	runner := func(_ context.Context, _ string, args ...string) (output, error) {
		if slices.Contains(args, "clone") || slices.Contains(args, "fetch") {
			return output{}, nil
		}

		if slices.Contains(args, "ls-tree") {
			return output{stdout: "160000 commit " + revision + "\tvendor/module\x00"}, nil
		}

		if slices.Contains(args, "show") {
			return output{stdout: "[submodule \"module\"]\n path = vendor/module\n url = file:///private/repo\n"}, nil
		}

		return output{}, errors.New("unexpected command")
	}
	reader := gitSkillReader{run: runner, root: t.TempDir(), files: make(map[string][]byte)}

	err := reader.read(t.Context(), "https://github.com/example/repo.git", revision, "", map[string]bool{}, 0)
	if err == nil || !strings.Contains(err.Error(), "unsupported Git submodule URL") {
		t.Fatalf("expected unsafe URL error, got %v", err)
	}
}

func TestGitFallbackAfterAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	revision := strings.Repeat("a", 40)
	runner := func(_ context.Context, _ string, args ...string) (output, error) {
		switch {
		case slices.Contains(args, "clone"), slices.Contains(args, "fetch"):
			return output{}, nil
		case slices.Contains(args, "ls-tree"):
			return output{stdout: "100644 blob x\tskills/visible/SKILL.md\x00100644 blob y\t.github/skills/hidden/SKILL.md\x00"}, nil
		case slices.Contains(args, "show"):
			return output{stdout: "---\nname: visible\ndescription: Visible.\n---\n"}, nil
		}

		return output{}, errors.New("unexpected command")
	}

	var warning string

	lister := githubTreeLister{
		client: server.Client(), apiBaseURL: server.URL, runGit: runner,
		warn: func(text string) { warning = text },
		listSkills: func(_ context.Context, dir string, _ bool) (output, error) {
			if _, err := os.Stat(filepath.Join(dir, "skills/visible/SKILL.md")); err != nil {
				t.Errorf("visible skill was not projected: %v", err)
			}

			if _, err := os.Stat(filepath.Join(dir, ".github/skills/hidden/SKILL.md")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("hidden skill was projected: %v", err)
			}

			return output{stdout: "◇  Available Skills\n│    visible\n└  Use --skill <name> to install specific skills\n"}, nil
		},
	}

	names, err := lister.listWithFallback(t.Context(), "example/repo", revision, false)
	if err != nil || strings.Join(names, ",") != "visible" || !strings.Contains(warning, "trying Git") {
		t.Fatalf("names %v, warning %q, error %v", names, warning, err)
	}

	lister.runGit = func(context.Context, string, ...string) (output, error) {
		return output{stderr: "fetch failed"}, errors.New("exit 1")
	}

	names, err = lister.listWithFallback(t.Context(), "example/repo", revision, false)
	if err == nil || len(names) != 0 || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "fetch failed") {
		t.Fatalf("names %v, error %v", names, err)
	}
}

func TestProjectedListingHonorsSkillPriority(t *testing.T) {
	if _, err := exec.LookPath("skills"); err != nil {
		t.Skip("skills CLI is not installed")
	}

	dir := t.TempDir()
	for _, file := range []struct{ path, name string }{
		{"skills/available/SKILL.md", "available"},
		{"docs/nested/SKILL.md", "not-discovered"},
	} {
		target := filepath.Join(dir, file.path)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(target, []byte("---\nname: "+file.name+"\ndescription: Example.\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	lister := githubTreeLister{}

	names, err := lister.listDirectory(t.Context(), dir, false)
	if err != nil || strings.Join(names, ",") != "available" {
		t.Fatalf("names %v, error %v", names, err)
	}
}

func TestProjectedListingReturnsEmptyWhenAllSkillsAreInternal(t *testing.T) {
	if _, err := exec.LookPath("skills"); err != nil {
		t.Skip("skills CLI is not installed")
	}

	dir := t.TempDir()

	target := filepath.Join(dir, "skills", "internal", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, []byte("---\nname: internal\ndescription: Internal.\nmetadata:\n  internal: true\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	names, err := (githubTreeLister{}).listDirectory(t.Context(), dir, false)
	if err != nil || len(names) != 0 {
		t.Fatalf("names %v, error %v", names, err)
	}

	names, err = (githubTreeLister{}).listDirectory(t.Context(), dir, true)
	if err != nil || strings.Join(names, ",") != "internal" {
		t.Fatalf("including internal: names %v, error %v", names, err)
	}

	if err := os.WriteFile(target, []byte("---\nname: invalid\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := (githubTreeLister{}).listDirectory(t.Context(), dir, false); err == nil {
		t.Fatal("invalid skill was silently treated as an empty listing")
	}
}
