package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

func (l githubTreeLister) listProjected(ctx context.Context, repo, revision string, paths []string, includeInternal bool) (names []string, err error) {
	apiURL := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", strings.TrimRight(l.apiBaseURL, "/"), repo, revision)

	data, err := l.get(ctx, apiURL, true, maxGitHubTreeSize)
	if err != nil {
		return nil, fmt.Errorf("fetch repository tree: %w", err)
	}

	var tree githubSkillTree
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("parse repository tree: %w", err)
	}

	if tree.Truncated {
		return nil, errors.New("GitHub returned a truncated repository tree")
	}

	wanted := make(map[string]bool, len(paths)+4)
	for _, p := range paths {
		wanted[p] = true
	}

	dir, err := os.MkdirTemp("", "skills-missing-")
	if err != nil {
		return nil, fmt.Errorf("create temporary directory: %w", err)
	}

	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary directory: %w", cleanupErr))
		}
	}()

	var files []githubTreeEntry

	for _, entry := range tree.Tree {
		if !wanted[entry.Path] && !projectionConfig(entry.Path) {
			continue
		}

		if entry.Type != "blob" || entry.Mode == "120000" {
			return nil, fmt.Errorf("unsupported file type for %s", entry.Path)
		}

		if !safeRepositoryPath(entry.Path) {
			return nil, fmt.Errorf("unsafe repository path %q", entry.Path)
		}

		files = append(files, entry)
		delete(wanted, entry.Path)
	}

	for _, p := range paths {
		if wanted[p] {
			return nil, fmt.Errorf("skill document %s missing from repository tree", p)
		}
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan githubTreeEntry)

	var (
		workers   sync.WaitGroup
		firstErr  error
		errorOnce sync.Once
	)

	for range min(githubSkillReadWorkers, len(files)) {
		workers.Add(1)
		go func() {
			defer workers.Done()

			for entry := range jobs {
				err := l.projectDocument(workCtx, repo, revision, dir, entry.Path)
				if err != nil {
					errorOnce.Do(func() {
						firstErr = err

						cancel()
					})

					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)

		for _, entry := range files {
			select {
			case jobs <- entry:
			case <-workCtx.Done():
				return
			}
		}
	}()

	workers.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return l.listDirectory(ctx, dir, includeInternal)
}

func (l githubTreeLister) projectDocument(ctx context.Context, repo, revision, dir, name string) error {
	content, err := l.rawDocument(ctx, repo, revision, name, maxSkillDocumentSize)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}

	return writeProjectedFile(dir, name, content)
}

func writeProjectedFile(dir, name string, content []byte) error {
	target := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create directory for %s: %w", name, err)
	}

	if err := os.WriteFile(target, content, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	return nil
}

func (l githubTreeLister) listDirectory(ctx context.Context, dir string, includeInternal bool) ([]string, error) {
	list := l.listSkills
	if list == nil {
		list = runSkillsList
	}

	out, err := list(ctx, dir, includeInternal)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && out.stderr == "" &&
			strings.Contains(out.stdout, "[SKILLS_NO_VALID_SKILLS]") {
			return []string{}, nil
		}

		return nil, fmt.Errorf("skills add --list: %w: %s", err, strings.TrimSpace(out.stderr+"\n"+out.stdout))
	}

	if out.stderr != "" && l.warn != nil {
		l.warn(out.stderr)
	}

	names, err := parseListing(out.stdout)
	if err != nil {
		return nil, err
	}

	return names, nil
}

func runSkillsList(ctx context.Context, dir string, includeInternal bool) (output, error) {
	cmd := skillsCommand(ctx, "add", dir, "--list")

	internal := "0"
	if includeInternal {
		internal = "1"
	}

	cmd.Env = append(cmd.Environ(), "NO_COLOR=1", "LC_ALL=C", "GIT_TERMINAL_PROMPT=0",
		"DO_NOT_TRACK=1", "INSTALL_INTERNAL_SKILLS="+internal)

	var stdout, stderr strings.Builder

	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	return output{stdout: stdout.String(), stderr: stderr.String()}, err
}

func parseListing(raw string) ([]string, error) {
	raw = ansiPattern.ReplaceAllString(raw, "")
	inside, done := false, false

	var names []string

	for line := range strings.SplitSeq(raw, "\n") {
		if strings.Contains(line, "Available Skills") {
			inside = true
			continue
		}

		if inside && strings.Contains(line, "Use --skill ") && strings.Contains(line, " to install specific skills") {
			done = true
			break
		}

		if inside && strings.HasPrefix(line, "│    ") {
			name := strings.TrimPrefix(line, "│    ")
			if name != "" && !strings.HasPrefix(name, " ") {
				names = append(names, name)
			}
		}
	}

	if !done || len(names) == 0 {
		return nil, errors.New("could not parse skill listing")
	}

	slices.Sort(names)

	return slices.Compact(names), nil
}
