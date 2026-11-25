package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

type gitSkillReader struct {
	run   commandRunner
	root  string
	files map[string][]byte
}

func (l githubTreeLister) listGit(ctx context.Context, repo, revision string, includeInternal bool) (names []string, err error) {
	temp, err := os.MkdirTemp("", "skills-missing-git-")
	if err != nil {
		return nil, fmt.Errorf("create temporary directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(temp); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary directory: %w", cleanupErr))
		}
	}()

	run := l.runGit
	if run == nil {
		run = executeGit
	}

	reader := gitSkillReader{run: run, root: temp, files: make(map[string][]byte)}
	if err := reader.read(ctx, "https://github.com/"+repo+".git", revision, "", make(map[string]bool), 0); err != nil {
		return nil, err
	}

	if !hasSkillFiles(reader.files) {
		return []string{}, nil
	}

	project := filepath.Join(temp, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		return nil, fmt.Errorf("create projection: %w", err)
	}

	for name, content := range reader.files {
		if err := writeProjectedFile(project, name, content); err != nil {
			return nil, err
		}
	}

	return l.listDirectory(ctx, project, includeInternal)
}

func executeGit(ctx context.Context, name string, args ...string) (output, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=0", "GIT_CONFIG_PARAMETERS=", "GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=", "SSH_ASKPASS=", "LC_ALL=C")

	var stdout, stderr strings.Builder

	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	return output{stdout: stdout.String(), stderr: stderr.String()}, err
}

func (r gitSkillReader) read(ctx context.Context, remote, revision, prefix string, ancestors map[string]bool, depth int) error {
	if !revisionPattern.MatchString(revision + " HEAD") {
		return fmt.Errorf("invalid Git revision %q", revision)
	}

	if depth > 8 {
		return errors.New("Git submodule nesting exceeds 8 levels")
	}

	key := remote + "@" + revision
	if ancestors[key] {
		return fmt.Errorf("cyclic Git submodule reference at %s", remote)
	}

	ancestors[key] = true
	defer delete(ancestors, key)

	temp, err := os.MkdirTemp(r.root, "repo-")
	if err != nil {
		return fmt.Errorf("create Git directory: %w", err)
	}

	dir := filepath.Join(temp, "repo")
	if _, err := r.git(ctx, "clone", "--no-checkout", "--no-recurse-submodules", "--filter=blob:none", "--depth=1", "--", remote, dir); err != nil {
		return fmt.Errorf("clone %s: %w", remote, err)
	}

	if _, err := r.git(ctx, "-C", dir, "fetch", "--no-tags", "--depth=1", "--filter=blob:none", "origin", revision); err != nil {
		return fmt.Errorf("fetch %s at %s: %w", remote, revision, err)
	}

	out, err := r.git(ctx, "-C", dir, "ls-tree", "-r", "-z", "--full-tree", revision)
	if err != nil {
		return fmt.Errorf("list Git tree for %s: %w", remote, err)
	}

	if len(out) > maxGitHubTreeSize {
		return fmt.Errorf("Git tree for %s exceeds %d bytes", remote, maxGitHubTreeSize)
	}

	var submodules []githubTreeEntry

	for entry := range strings.SplitSeq(out, "\x00") {
		if entry == "" {
			continue
		}

		header, name, ok := strings.Cut(entry, "\t")

		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || !safeRepositoryPath(name) {
			return fmt.Errorf("invalid Git tree entry in %s", remote)
		}

		fullPath := path.Join(prefix, name)
		if fields[1] == "commit" {
			if !hasHiddenDirectory(fullPath) && !hasSkippedDirectory(fullPath) {
				submodules = append(submodules, githubTreeEntry{Path: name, SHA: fields[2]})
			}

			continue
		}

		isSkill := pathBase(name) == "SKILL.md" && !hasHiddenDirectory(path.Dir(fullPath)) && !hasSkippedDirectory(fullPath)
		if !isSkill && !(prefix == "" && projectionConfig(name)) {
			continue
		}

		if fields[1] != "blob" || fields[0] == "120000" {
			return fmt.Errorf("unsupported file type for %s", fullPath)
		}

		content, err := r.git(ctx, "-C", dir, "show", revision+":"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", fullPath, err)
		}

		if len(content) > maxSkillDocumentSize {
			return fmt.Errorf("%s exceeds %d bytes", fullPath, maxSkillDocumentSize)
		}

		r.files[fullPath] = []byte(content)
	}

	if len(submodules) == 0 {
		return nil
	}

	modules, err := r.git(ctx, "-C", dir, "show", revision+":.gitmodules")
	if err != nil {
		return fmt.Errorf("read .gitmodules from %s: %w", remote, err)
	}

	if len(modules) > maxSkillDocumentSize {
		return fmt.Errorf(".gitmodules exceeds %d bytes", maxSkillDocumentSize)
	}

	configs, err := parseGitmodules([]byte(modules))
	if err != nil {
		return fmt.Errorf("parse .gitmodules from %s: %w", remote, err)
	}

	for _, module := range submodules {
		config, ok := configs[module.Path]
		if !ok {
			return fmt.Errorf("%s is missing from .gitmodules", module.Path)
		}

		moduleURL, err := resolveGitURL(remote, config.URL)
		if err != nil {
			return fmt.Errorf("resolve submodule %s: %w", module.Path, err)
		}

		if err := r.read(ctx, moduleURL, module.SHA, path.Join(prefix, module.Path), ancestors, depth+1); err != nil {
			return fmt.Errorf("read submodule %s: %w", module.Path, err)
		}
	}

	return nil
}

func (r gitSkillReader) git(ctx context.Context, args ...string) (string, error) {
	options := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "core.hooksPath=" + os.DevNull}

	out, err := r.run(ctx, "git", append(options, args...)...)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out.stderr))
	}

	return out.stdout, nil
}

func resolveGitURL(parent, value string) (string, error) {
	if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		base, err := url.Parse(parent)
		if err != nil {
			return "", err
		}

		base.Path = path.Clean(path.Join(base.Path, value))
		value = base.String()
	}

	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") ||
		net.ParseIP(u.Hostname()) != nil || strings.EqualFold(u.Hostname(), "localhost") ||
		strings.HasSuffix(strings.ToLower(u.Hostname()), ".localhost") ||
		strings.HasSuffix(strings.ToLower(u.Hostname()), ".local") {
		return "", fmt.Errorf("unsupported Git submodule URL %q", value)
	}

	return u.String(), nil
}

func safeRepositoryPath(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "\\") && name == path.Clean(name) &&
		!strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../")
}

func projectionConfig(name string) bool {
	return slices.Contains([]string{"skills.sh.json", "skills-lock.json", ".claude-plugin/plugin.json", ".claude-plugin/marketplace.json"}, name)
}

func hasSkillFiles(files map[string][]byte) bool {
	for name := range files {
		if pathBase(name) == "SKILL.md" {
			return true
		}
	}

	return false
}
