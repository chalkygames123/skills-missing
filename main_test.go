package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartupCommandsRunConcurrentlyAndReportDiagnosticsInOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex

	started := 0
	ready := make(chan struct{})
	runner := func(ctx context.Context, name string, args ...string) (output, error) {
		if name == "git" {
			return output{stdout: strings.Repeat("a", 40) + "\tHEAD\n"}, nil
		}

		if name != "skills" {
			return output{}, fmt.Errorf("unexpected command: %s %v", name, args)
		}

		command := strings.Join(args, " ")
		switch command {
		case "--version", "list --json", "list --json -g":
			mu.Lock()

			started++
			if started == 3 {
				close(ready)
			}
			mu.Unlock()

			select {
			case <-ready:
			case <-ctx.Done():
				return output{}, ctx.Err()
			}

			switch command {
			case "--version":
				return output{stdout: "1.0.0", stderr: "version warning"}, nil
			case "list --json":
				return output{stdout: "[]", stderr: "project warning"}, nil
			default:
				return output{stdout: "[]", stderr: "global warning"}, nil
			}
		default:
			return output{}, fmt.Errorf("unexpected command: %s %v", name, args)
		}
	}

	var stdout, stderr bytes.Buffer

	a := app{run: runner, cacheDir: t.TempDir(), warn: &stderr,
		listRemoteSkills: func(context.Context, string, string, bool) ([]string, error) {
			return []string{"installed", "missing"}, nil
		}}
	if err := a.execute(ctx, []string{"example/repo"}, &stdout); err != nil {
		t.Fatal(err)
	}

	if got, want := stdout.String(), "example/repo\tinstalled\nexample/repo\tmissing\n"; got != want {
		t.Fatalf("stdout %q, want %q", got, want)
	}

	if got, want := stderr.String(), "version warning\nproject warning\nglobal warning\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
}

func TestStartupFailuresReturnFirstErrorWithoutOutput(t *testing.T) {
	for _, test := range []struct {
		name        string
		versionErr  bool
		projectErr  bool
		globalErr   bool
		wantCommand string
	}{
		{"version before project", true, true, true, "--version"},
		{"project before global", false, true, true, "list --json"},
		{"global failure", false, false, true, "list --json -g"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := func(_ context.Context, name string, args ...string) (output, error) {
				command := strings.Join(args, " ")
				switch command {
				case "--version":
					if test.versionErr {
						return output{stderr: "version failed"}, errors.New("exit 1")
					}

					return output{stdout: "1.0.0"}, nil
				case "list --json":
					if test.projectErr {
						return output{stderr: "project failed"}, errors.New("exit 1")
					}

					return output{stdout: "[]"}, nil
				case "list --json -g":
					if test.globalErr {
						return output{stderr: "global failed"}, errors.New("exit 1")
					}

					return output{stdout: "[]"}, nil
				default:
					return output{}, fmt.Errorf("unexpected command: %s %v", name, args)
				}
			}

			var stdout bytes.Buffer

			a := app{run: runner, cacheDir: t.TempDir(), warn: &bytes.Buffer{}}

			err := a.execute(t.Context(), []string{"example/repo"}, &stdout)
			if err == nil || !strings.Contains(err.Error(), "skills "+test.wantCommand+":") || stdout.Len() != 0 {
				t.Fatalf("output %q, error %v", stdout.String(), err)
			}
		})
	}
}

func TestAutomaticSourcesAndNoPartialOutput(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex

	active, maxActive := 0, 0
	failed := false
	runner := func(ctx context.Context, name string, args ...string) (output, error) {
		if name == "git" {
			return output{stdout: strings.Repeat("a", 40) + "\tHEAD\n"}, nil
		}

		switch strings.Join(args, " ") {
		case "--version":
			return output{stdout: "1.0.0\n"}, nil
		case "list --json":
			return output{stdout: `[{"name":"installed","source":"a/one","sourceType":"github","status":"tracked"},{"name":"renamed","renamedFrom":"installed","source":"b/two","sourceType":"github","status":"tracked"}]`}, nil
		case "list --json -g":
			return output{stdout: "[]"}, nil
		}

		return output{}, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	listRemote := func(_ context.Context, repo, _ string, _ bool) ([]string, error) {
		mu.Lock()

		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()

		if failed && repo == "b/two" {
			return nil, errors.New("fetch failed")
		}

		return []string{"installed", "missing"}, nil
	}

	var warnings, stdout bytes.Buffer

	a := app{run: runner, listRemoteSkills: listRemote, cacheDir: dir, warn: &warnings}
	if err := a.execute(t.Context(), nil, &stdout); err != nil {
		t.Fatal(err)
	}

	if got, want := stdout.String(), "a/one\tmissing\nb/two\tmissing\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if maxActive < 2 {
		t.Fatalf("lookups did not overlap: %d", maxActive)
	}

	failed = true
	a.cacheDir = t.TempDir()

	stdout.Reset()

	if err := a.execute(t.Context(), nil, &stdout); err == nil || stdout.Len() != 0 || !strings.Contains(err.Error(), "fetch failed") {
		t.Fatalf("failure: output %q, error %v", stdout.String(), err)
	}
}

func TestIgnoreFileFiltersOnlyMatchingSourceAndSkill(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	ignoreFile := filepath.Join(configHome, "skills-missing", "ignore")
	if err := os.MkdirAll(filepath.Dir(ignoreFile), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(ignoreFile, []byte("a/one\tshared\na/one\tgone\nunused/repo\tgone\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := func(ctx context.Context, name string, args ...string) (output, error) {
		if name == "git" {
			return output{stdout: strings.Repeat("a", 40) + "\tHEAD\n"}, nil
		}

		switch strings.Join(args, " ") {
		case "--version":
			return output{stdout: "1.0.0\n"}, nil
		case "list --json":
			return output{stdout: `[{"name":"installed","source":"a/one","sourceType":"github","status":"tracked"},{"name":"installed","source":"b/two","sourceType":"github","status":"tracked"}]`}, nil
		case "list --json -g":
			return output{stdout: "[]"}, nil
		}

		return output{}, fmt.Errorf("unexpected command: %s %v", name, args)
	}

	var stdout, warnings bytes.Buffer

	a := app{run: runner, cacheDir: t.TempDir(), warn: &warnings,
		listRemoteSkills: func(context.Context, string, string, bool) ([]string, error) {
			return []string{"shared", "visible"}, nil
		}}
	if err := a.execute(t.Context(), []string{"--ignore-file", ignoreFile}, &stdout); err != nil {
		t.Fatal(err)
	}

	if got, want := stdout.String(), "a/one\tvisible\nb/two\tshared\nb/two\tvisible\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if !strings.Contains(warnings.String(), "Ignore file entry refers to unavailable skill: a/one\tgone") ||
		strings.Contains(warnings.String(), "a/one\tshared") ||
		strings.Contains(warnings.String(), "unused/repo\tgone") {
		t.Fatalf("unexpected warnings %q", warnings.String())
	}

	stdout.Reset()
	warnings.Reset()

	if err := a.execute(t.Context(), nil, &stdout); err != nil {
		t.Fatal(err)
	}

	if got, want := stdout.String(), "a/one\tvisible\nb/two\tshared\nb/two\tvisible\n"; got != want {
		t.Fatalf("default ignore file: got %q, want %q", got, want)
	}

	if !strings.Contains(warnings.String(), "Ignore file entry refers to unavailable skill: a/one\tgone") {
		t.Fatalf("default ignore file warning: %q", warnings.String())
	}
}

func TestInvalidIgnoreFileFailsWithoutOutput(t *testing.T) {
	ignoreFile := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ignoreFile, []byte("legacy-name-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := app{run: func(context.Context, string, ...string) (output, error) {
		return output{}, errors.New("unexpected command")
	}, warn: &bytes.Buffer{}}

	var stdout bytes.Buffer

	err := a.execute(t.Context(), []string{"--ignore-file", ignoreFile}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "line 1") || stdout.Len() != 0 {
		t.Fatalf("output %q, error %v", stdout.String(), err)
	}
}

func TestSkippedSkillDiagnosticIsSuppressedButOtherDiagnosticsRemain(t *testing.T) {
	const skippedSkill = "[SKILLS_SKILL_SKIPPED] ⚠ Skipped /tmp/skills/SKILL.md — YAML parse error: invalid mapping\n\ndescription: bad value\n             ^\n"

	runner := func(ctx context.Context, name string, args ...string) (output, error) {
		if name == "git" {
			return output{stdout: strings.Repeat("a", 40) + "\tHEAD\n"}, nil
		}

		switch strings.Join(args, " ") {
		case "--version":
			return output{stdout: "1.0.0\n", stderr: skippedSkill + "Warning: preserved diagnostic\n"}, nil
		case "list --json", "list --json -g":
			return output{stdout: "[]"}, nil
		}

		return output{}, fmt.Errorf("unexpected command: %s %v", name, args)
	}

	ignoreFile := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ignoreFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer

	a := app{run: runner, cacheDir: t.TempDir(), warn: &stderr,
		listRemoteSkills: func(context.Context, string, string, bool) ([]string, error) {
			return []string{"installed", "missing"}, nil
		}}
	if err := a.execute(t.Context(), []string{"--ignore-file", ignoreFile, "example/repo"}, &stdout); err != nil {
		t.Fatal(err)
	}

	if got, want := stdout.String(), "example/repo\tinstalled\nexample/repo\tmissing\n"; got != want {
		t.Fatalf("stdout %q, want %q", got, want)
	}

	if got, want := stderr.String(), "Warning: preserved diagnostic\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
}

func TestCacheInvalidationAndWarnings(t *testing.T) {
	dir := t.TempDir()
	revision, version := strings.Repeat("a", 40), "1.0.0"
	fetches := 0
	runner := func(ctx context.Context, name string, args ...string) (output, error) {
		if name == "git" {
			return output{stdout: revision + "\tHEAD\n"}, nil
		}

		switch strings.Join(args, " ") {
		case "--version":
			return output{stdout: version}, nil
		case "list --json", "list --json -g":
			return output{stdout: "[]"}, nil
		}

		return output{}, errors.New("unexpected command")
	}

	var stderr bytes.Buffer

	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	a := app{run: runner, cacheDir: dir, warn: &stderr, now: func() time.Time { return now },
		listRemoteSkills: func(context.Context, string, string, bool) ([]string, error) {
			fetches++
			return []string{"installed", "missing"}, nil
		}}
	run := func(want int) {
		t.Helper()

		var stdout bytes.Buffer
		if err := a.execute(t.Context(), []string{"example/skills"}, &stdout); err != nil {
			t.Fatal(err)
		}

		if got := stdout.String(); got != "example/skills\tinstalled\nexample/skills\tmissing\n" {
			t.Fatalf("output %q", got)
		}

		if fetches != want {
			t.Fatalf("fetches %d, want %d", fetches, want)
		}
	}
	run(1)
	run(1)

	now = now.Add(cacheRevalidationInterval)
	revision = strings.Repeat("b", 40)

	run(2)

	version = "2.0.0"

	run(3)

	a.internal = "1"

	run(4)
}

func TestFreshCacheSkipsRemoteRevisionLookup(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	entry := cacheEntry{
		Format:    cacheFormatVersion,
		Repo:      "example/repo",
		Revision:  strings.Repeat("a", 40),
		Version:   "1.0.0",
		CheckedAt: now.Add(-time.Minute),
		Names:     []string{"cached-skill"},
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}

	cachePath := filepath.Join(dir, "example@repo.json")
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	commands := 0
	a := app{
		run: func(context.Context, string, ...string) (output, error) {
			commands++
			return output{}, errors.New("unexpected command")
		},
		cacheDir: dir,
		warn:     &bytes.Buffer{},
		now:      func() time.Time { return now },
	}

	names, err := a.available(t.Context(), "example/repo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	if got, want := strings.Join(names, ","), "cached-skill"; got != want {
		t.Fatalf("names %q, want %q", got, want)
	}

	if commands != 0 {
		t.Fatalf("ran %d commands with a fresh cache", commands)
	}
}

func TestLegacyCacheRefreshesTimestampAfterRevisionCheck(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	revision := strings.Repeat("b", 40)
	entry := cacheEntry{
		Format:   cacheFormatVersion,
		Repo:     "example/repo",
		Revision: revision,
		Version:  "1.0.0",
		Names:    []string{"cached-skill"},
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}

	cachePath := filepath.Join(dir, "example@repo.json")
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	commands := 0
	a := app{
		run: func(_ context.Context, name string, _ ...string) (output, error) {
			commands++

			if name != "git" {
				t.Fatalf("unexpected command %s", name)
			}

			return output{stdout: revision + "\tHEAD\n"}, nil
		},
		cacheDir: dir,
		warn:     &bytes.Buffer{},
		now:      func() time.Time { return now },
	}

	names, err := a.available(t.Context(), "example/repo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	if got, want := strings.Join(names, ","), "cached-skill"; got != want {
		t.Fatalf("names %q, want %q", got, want)
	}

	if commands != 1 {
		t.Fatalf("ran %d commands, want one revision check", commands)
	}

	data, err = os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}

	if !entry.CheckedAt.Equal(now) {
		t.Fatalf("checkedAt %s, want %s", entry.CheckedAt, now)
	}
}
