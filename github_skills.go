package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v4"
)

const (
	githubAPIBaseURL       = "https://api.github.com"
	githubRawBaseURL       = "https://raw.githubusercontent.com"
	maxGitHubTreeSize      = 20 << 20
	maxSkillDocumentSize   = 1 << 20
	githubSkillReadWorkers = 4
)

var skillMetadataEscapePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\x1b\][\s\S]*?(?:\x07|\x1b\\)`),
	regexp.MustCompile(`\x1b[P^_][\s\S]*?\x1b\\`),
	regexp.MustCompile(`\x1b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]`),
	regexp.MustCompile(`\x1b[\x20-\x7e]`),
	regexp.MustCompile(`[\x80-\x9f]`),
	regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`),
}

var skillMetadataNewlines = regexp.MustCompile(`[\r\n]+`)

type remoteSkillLister func(context.Context, string, string, bool) ([]string, error)

type githubSkillTree struct {
	Tree      []githubTreeEntry `json:"tree"`
	Truncated bool              `json:"truncated"`
}

type githubTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Mode string `json:"mode"`
}

type githubTreeLister struct {
	client     *http.Client
	apiBaseURL string
	rawBaseURL string
	token      string
	listSkills func(context.Context, string, bool) (output, error)
	runGit     commandRunner
	warn       func(string)
}

func newGitHubTreeLister(token string, warn func(string)) remoteSkillLister {
	lister := githubTreeLister{
		client:     &http.Client{Timeout: 20 * time.Second},
		apiBaseURL: githubAPIBaseURL,
		rawBaseURL: githubRawBaseURL,
		token:      token,
		warn:       warn,
	}

	return lister.listWithFallback
}

func (l githubTreeLister) listWithFallback(ctx context.Context, repo, revision string, includeInternal bool) ([]string, error) {
	names, apiErr := l.list(ctx, repo, revision, includeInternal)
	if apiErr == nil {
		return names, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if l.warn != nil {
		l.warn(fmt.Sprintf("Could not list %s through the GitHub tree API; trying Git: %v", repo, apiErr))
	}

	names, gitErr := l.listGit(ctx, repo, revision, includeInternal)
	if gitErr != nil {
		return nil, errors.Join(fmt.Errorf("GitHub tree API: %w", apiErr), fmt.Errorf("Git lookup: %w", gitErr))
	}

	return names, nil
}

func githubTokenFromEnvironment() string {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}

	return ""
}

func (l githubTreeLister) list(ctx context.Context, repo, revision string, includeInternal bool) ([]string, error) {
	paths, err := l.skillDocumentPaths(ctx, repo, revision, make(map[string]bool), 0)
	if err != nil {
		return nil, err
	}

	rootIndex := slices.Index(paths, "SKILL.md")
	if rootIndex >= 0 {
		name, internal, err := l.skillName(ctx, repo, revision, paths[rootIndex])
		if err != nil {
			return nil, fmt.Errorf("parse root skill document: %w", err)
		}

		if includeInternal || !internal {
			return []string{name}, nil
		}

		paths = append(paths[:rootIndex], paths[rootIndex+1:]...)
	}

	for _, skillPath := range paths {
		if !supportedSkillPath(skillPath) {
			return l.listProjected(ctx, repo, revision, paths, includeInternal)
		}
	}

	names := make([]string, len(paths))

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)

	var (
		workers   sync.WaitGroup
		errorOnce sync.Once
		firstErr  error
	)

	for range min(githubSkillReadWorkers, len(paths)) {
		workers.Add(1)
		go func() {
			defer workers.Done()

			for index := range jobs {
				name, internal, err := l.skillName(workCtx, repo, revision, paths[index])
				if err != nil {
					errorOnce.Do(func() {
						firstErr = fmt.Errorf("read %s: %w", paths[index], err)

						cancel()
					})

					return
				}

				if !internal || includeInternal {
					names[index] = name
				}
			}
		}()
	}

	go func() {
		defer close(jobs)

		for index := range paths {
			select {
			case jobs <- index:
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

	names = slices.DeleteFunc(names, func(name string) bool { return name == "" })
	slices.Sort(names)
	names = slices.Compact(names)

	return names, nil
}

func (l githubTreeLister) skillName(ctx context.Context, repo, revision, skillPath string) (string, bool, error) {
	data, err := l.rawDocument(ctx, repo, revision, skillPath, maxSkillDocumentSize)
	if err != nil {
		return "", false, err
	}

	return parseSkillDocumentName(data)
}

func (l githubTreeLister) rawDocument(ctx context.Context, repo, revision, filePath string, limit int64) ([]byte, error) {
	escapedPath := make([]string, 0, strings.Count(filePath, "/")+1)
	for _, part := range strings.Split(filePath, "/") {
		escapedPath = append(escapedPath, url.PathEscape(part))
	}

	rawURL := fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(l.rawBaseURL, "/"), repo, url.PathEscape(revision), strings.Join(escapedPath, "/"))

	return l.get(ctx, rawURL, false, limit)
}

func (l githubTreeLister) skillDocumentPaths(ctx context.Context, repo, revision string, ancestors map[string]bool, depth int) ([]string, error) {
	if depth > 8 {
		return nil, errors.New("Git submodule nesting exceeds 8 levels")
	}

	key := repo + "@" + revision
	if ancestors[key] {
		return nil, fmt.Errorf("cyclic Git submodule reference at %s", repo)
	}

	ancestors[key] = true
	defer delete(ancestors, key)

	apiURL := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", strings.TrimRight(l.apiBaseURL, "/"), repo, url.PathEscape(revision))

	data, err := l.get(ctx, apiURL, true, maxGitHubTreeSize)
	if err != nil {
		return nil, fmt.Errorf("fetch repository tree for %s: %w", repo, err)
	}

	var tree githubSkillTree
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("parse repository tree for %s: %w", repo, err)
	}

	if tree.Truncated {
		return nil, fmt.Errorf("GitHub returned a truncated repository tree for %s", repo)
	}

	paths := treeSkillDocumentPaths(tree.Tree)

	var submodules []githubTreeEntry

	for _, entry := range tree.Tree {
		if entry.Type == "commit" && !hasSkippedDirectory(entry.Path) && !hasHiddenDirectory(entry.Path) {
			submodules = append(submodules, entry)
		}
	}

	if len(submodules) == 0 {
		return paths, nil
	}

	var gitmodulesFound bool

	for _, entry := range tree.Tree {
		if entry.Path == ".gitmodules" && entry.Type == "blob" {
			gitmodulesFound = true
			break
		}
	}

	if !gitmodulesFound {
		return nil, fmt.Errorf("%s has Git submodules but no .gitmodules file", repo)
	}

	gitmodules, err := l.rawDocument(ctx, repo, revision, ".gitmodules", maxSkillDocumentSize)
	if err != nil {
		return nil, fmt.Errorf("read %s/.gitmodules: %w", repo, err)
	}

	submoduleURLs, err := parseGitmodules(gitmodules)
	if err != nil {
		return nil, fmt.Errorf("parse %s/.gitmodules: %w", repo, err)
	}

	for _, submodule := range submodules {
		submoduleConfig, ok := submoduleURLs[submodule.Path]
		if !ok {
			return nil, fmt.Errorf("%s is missing from %s/.gitmodules", submodule.Path, repo)
		}

		submoduleRepo, err := resolveSubmoduleRepo(repo, submoduleConfig.URL)
		if err != nil {
			return nil, fmt.Errorf("resolve Git submodule %s: %w", submodule.Path, err)
		}

		submodulePaths, err := l.skillDocumentPaths(ctx, submoduleRepo, submodule.SHA, ancestors, depth+1)
		if err != nil {
			return nil, err
		}

		if len(submodulePaths) > 0 {
			return nil, fmt.Errorf("Git submodule %s contains skill documents", submodule.Path)
		}
	}

	return paths, nil
}

func (l githubTreeLister) get(ctx context.Context, address string, api bool, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}

	request.Header.Set("User-Agent", "skills-missing")

	if api {
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}

	if l.token != "" {
		request.Header.Set("Authorization", "Bearer "+l.token)
	}

	client := l.client
	if client == nil {
		client = http.DefaultClient
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		if api && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests) {
			details := []string{"no token provided"}
			if l.token != "" {
				details[0] = "token provided"
			}

			if response.StatusCode == http.StatusUnauthorized {
				details = append(details, "authentication required or rejected")
			}

			if response.StatusCode == http.StatusTooManyRequests {
				details = append(details, "API rate limited")
			}

			if response.Header.Get("X-RateLimit-Remaining") == "0" {
				details = append(details, "API rate limit remaining: 0")
				if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
					details = append(details, "resets at "+time.Unix(reset, 0).UTC().Format(time.RFC3339))
				}
			}

			if retry, err := strconv.ParseUint(response.Header.Get("Retry-After"), 10, 32); err == nil {
				details = append(details, fmt.Sprintf("retry after %d seconds", retry))
			}

			return nil, fmt.Errorf("GitHub returned HTTP %s (%s)", response.Status, strings.Join(details, "; "))
		}

		return nil, fmt.Errorf("GitHub returned HTTP %s", response.Status)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}

	return data, nil
}

func treeSkillDocumentPaths(entries []githubTreeEntry) []string {
	var paths []string

	for _, entry := range entries {
		if entry.Type != "blob" || pathBase(entry.Path) != "SKILL.md" ||
			hasSkippedDirectory(entry.Path) || hasHiddenDirectory(path.Dir(entry.Path)) {
			continue
		}

		paths = append(paths, entry.Path)
	}

	slices.Sort(paths)

	return paths
}

type gitSubmodule struct {
	Path string
	URL  string
}

func parseGitmodules(data []byte) (map[string]gitSubmodule, error) {
	submodules := make(map[string]gitSubmodule)

	var current string

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			if !strings.HasPrefix(section, "submodule ") {
				current = ""
				continue
			}

			name, err := strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(section, "submodule")))
			if err != nil || name == "" {
				return nil, errors.New("invalid submodule section")
			}

			submodules[name] = gitSubmodule{}
			current = name

			continue
		}

		if current == "" {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("invalid quoted submodule value: %w", err)
			}

			value = unquoted
		} else if comment := strings.IndexAny(value, "#;"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}

		submodule := submodules[current]

		switch strings.TrimSpace(key) {
		case "path":
			submodule.Path = value
		case "url":
			submodule.URL = value
		}

		submodules[current] = submodule
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	byPath := make(map[string]gitSubmodule, len(submodules))
	for _, submodule := range submodules {
		if submodule.Path == "" || submodule.URL == "" {
			return nil, errors.New("submodule requires both path and URL")
		}

		byPath[submodule.Path] = submodule
	}

	return byPath, nil
}

func resolveSubmoduleRepo(parentRepo, submoduleURL string) (string, error) {
	if !strings.HasPrefix(submoduleURL, "../") && !strings.HasPrefix(submoduleURL, "./") {
		if repo := normalizeRepo(submoduleURL); repo != "" {
			return repo, nil
		}

		return "", fmt.Errorf("unsupported GitHub URL %q", submoduleURL)
	}

	repo := normalizeRepo(path.Clean(path.Join(parentRepo, submoduleURL)))
	if repo == "" {
		return "", fmt.Errorf("unsupported relative GitHub URL %q", submoduleURL)
	}

	return repo, nil
}

func pathBase(value string) string {
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		return value[index+1:]
	}

	return value
}

func hasSkippedDirectory(value string) bool {
	for _, part := range strings.Split(value, "/") {
		switch part {
		case "node_modules", ".git", "dist", "build", "__pycache__":
			return true
		}
	}

	return false
}

func hasHiddenDirectory(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part != "." && strings.HasPrefix(part, ".") {
			return true
		}
	}

	return false
}

func supportedSkillPath(skillPath string) bool {
	parts := strings.Split(skillPath, "/")
	if len(parts) == 2 || len(parts) == 3 && parts[0] == "skills" {
		return true
	}

	for index, part := range parts {
		if part != "skills" || index == 0 || index+3 != len(parts) || parts[index+2] != "SKILL.md" {
			continue
		}

		prefix := parts[:index]
		if len(prefix) == 1 && (prefix[0] == "agent" || prefix[0] == "data") {
			return true
		}
	}

	return false
}

func parseSkillDocumentName(data []byte) (string, bool, error) {
	content := string(data)
	if after, ok := strings.CutPrefix(content, "\uFEFF"); ok {
		content = after
	}

	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", false, errors.New("missing YAML frontmatter")
	}

	start := strings.IndexByte(content, '\n') + 1
	end := -1

	for lineStart := start; lineStart < len(content); {
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += lineStart
		}

		line := strings.TrimSuffix(content[lineStart:lineEnd], "\r")
		if line == "---" {
			end = lineStart
			break
		}

		if lineEnd == len(content) {
			break
		}

		lineStart = lineEnd + 1
	}

	if end < 0 {
		return "", false, errors.New("unterminated YAML frontmatter")
	}

	var metadata struct {
		Name        any            `yaml:"name"`
		Description any            `yaml:"description"`
		Metadata    map[string]any `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(content[start:end]), &metadata); err != nil {
		return "", false, fmt.Errorf("parse YAML frontmatter: %w", err)
	}

	name, nameOK := metadata.Name.(string)
	description, descriptionOK := metadata.Description.(string)
	name = sanitizeSkillMetadata(name)

	description = sanitizeSkillMetadata(description)
	if !nameOK || !descriptionOK || name == "" || description == "" {
		return "", false, errors.New("skill requires non-empty string name and description")
	}

	internal := false

	if value, ok := metadata.Metadata["internal"]; ok {
		switch value := value.(type) {
		case bool:
			internal = value
		case string:
			internal = value == "true" || value == "1"
		}
	}

	return name, internal, nil
}

func sanitizeSkillMetadata(value string) string {
	for _, pattern := range skillMetadataEscapePatterns {
		value = pattern.ReplaceAllString(value, "")
	}

	return strings.TrimSpace(skillMetadataNewlines.ReplaceAllString(value, " "))
}
