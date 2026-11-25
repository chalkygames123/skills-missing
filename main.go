package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const workers = 4
const cacheRevalidationInterval = 5 * time.Minute
const cacheFormatVersion = 2

var (
	repositoryPattern = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)
	revisionPattern   = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})\s+HEAD$`)
	ansiPattern       = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

type installedSkill struct {
	Name        string  `json:"name"`
	Source      *string `json:"source"`
	SourceType  string  `json:"sourceType"`
	RenamedFrom *string `json:"renamedFrom"`
	Status      string  `json:"status"`
}

type cacheEntry struct {
	Format    int       `json:"format"`
	Repo      string    `json:"repo"`
	Revision  string    `json:"revision"`
	Version   string    `json:"version"`
	Internal  string    `json:"internal"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	Names     []string  `json:"names"`
}

type output struct {
	stdout string
	stderr string
}

type commandRunner func(context.Context, string, ...string) (output, error)

type app struct {
	run              commandRunner
	listRemoteSkills remoteSkillLister
	cacheDir         string
	internal         string
	warn             io.Writer
	warnMu           sync.Mutex
	now              func() time.Time
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cacheBase := os.Getenv("XDG_CACHE_HOME")
	if cacheBase == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		cacheBase = filepath.Join(home, ".cache")
	}

	a := app{
		run:      execute,
		cacheDir: filepath.Join(cacheBase, "skills-missing"),
		internal: os.Getenv("INSTALL_INTERNAL_SKILLS"),
		warn:     os.Stderr,
	}

	a.listRemoteSkills = newGitHubTreeLister(githubTokenFromEnvironment(), a.commandWarning)
	if err := a.execute(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "skills-missing:", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, name string, args ...string) (output, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if name == "skills" {
		cmd = skillsCommand(ctx, args...)
	}

	cmd.Env = append(cmd.Environ(), "NO_COLOR=1", "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")

	var stdout, stderr strings.Builder

	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	return output{stdout.String(), stderr.String()}, err
}

func (a *app) warning(text string) {
	if text == "" {
		return
	}

	a.warnMu.Lock()
	defer a.warnMu.Unlock()

	fmt.Fprint(a.warn, text)

	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(a.warn)
	}
}

func (a *app) commandWarning(text string) {
	text = ansiPattern.ReplaceAllString(text, "")

	var lines []string

	skippingSkippedSkill := false

	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[SKILLS_SKILL_SKIPPED]") {
			skippingSkippedSkill = true
			continue
		}

		if skippingSkippedSkill {
			if trimmed == "^" {
				skippingSkippedSkill = false
			}

			continue
		}

		lines = append(lines, line)
	}

	a.warning(strings.TrimSpace(strings.Join(lines, "\n")))
}

func (a *app) command(ctx context.Context, name string, args ...string) (output, error) {
	out, err := a.run(ctx, name, args...)
	if err != nil {
		detail := strings.TrimSpace(out.stderr + "\n" + out.stdout)
		if detail != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
		}

		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return out, nil
}

func normalizeRepo(source string) string {
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		if len(source) >= len(prefix) && strings.EqualFold(source[:len(prefix)], prefix) {
			source = source[len(prefix):]
			break
		}
	}

	source = strings.ToLower(strings.TrimSuffix(source, ".git"))
	if !repositoryPattern.MatchString(source) {
		return ""
	}

	return source
}

func (a *app) execute(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("skills-missing", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var ignoreFile string

	ignoreFileSet := false

	flags.Func("ignore-file", "ignore file", func(value string) error {
		ignoreFile, ignoreFileSet = value, true
		return nil
	})

	if err := flags.Parse(args); err != nil {
		return err
	}

	args = flags.Args()
	if len(args) > 1 {
		return errors.New("usage: skills-missing [--ignore-file PATH] [owner/repository]")
	}

	var selected string
	if len(args) == 1 {
		selected = strings.ToLower(args[0])
		if !repositoryPattern.MatchString(selected) {
			return errors.New("repository must use owner/repository")
		}
	}

	if !ignoreFileSet {
		var err error

		ignoreFile, err = defaultIgnoreFile()
		if err != nil {
			return err
		}
	}

	ignored, err := readIgnored(ignoreFile, ignoreFileSet)
	if err != nil {
		return fmt.Errorf("read ignore file %s: %w", ignoreFile, err)
	}

	type commandResult struct {
		output output
		err    error
	}

	commands := [][]string{
		{"--version"},
		{"list", "--json"},
		{"list", "--json", "-g"},
	}
	startupResults := make([]commandResult, len(commands))

	var commandWorkers sync.WaitGroup
	for i, args := range commands {
		commandWorkers.Add(1)
		go func() {
			defer commandWorkers.Done()

			startupResults[i].output, startupResults[i].err = a.command(ctx, "skills", args...)
		}()
	}

	commandWorkers.Wait()

	version := startupResults[0].output
	if startupResults[0].err != nil {
		return startupResults[0].err
	}

	a.commandWarning(version.stderr)

	if err := os.MkdirAll(a.cacheDir, 0700); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	installed := make(map[string]map[string]bool)

	for _, result := range startupResults[1:] {
		if result.err != nil {
			return result.err
		}

		out := result.output
		a.commandWarning(out.stderr)

		var skills []installedSkill
		if err := json.Unmarshal([]byte(out.stdout), &skills); err != nil || skills == nil {
			return fmt.Errorf("parse installed skills: invalid JSON array")
		}

		for _, skill := range skills {
			if skill.Status == "missing" || skill.SourceType != "github" || skill.Source == nil {
				continue
			}

			repo := normalizeRepo(*skill.Source)
			if repo == "" {
				continue
			}

			name := skill.Name
			if skill.RenamedFrom != nil {
				name = *skill.RenamedFrom
			}

			if installed[repo] == nil {
				installed[repo] = make(map[string]bool)
			}

			installed[repo][name] = true
		}
	}

	repos := make([]string, 0, len(installed))
	if selected != "" {
		repos = append(repos, selected)
	} else {
		for repo := range installed {
			repos = append(repos, repo)
		}

		if len(repos) == 0 {
			return errors.New("no GitHub source repositories found")
		}
	}

	slices.Sort(repos)

	type result struct {
		repo  string
		names []string
		err   error
	}

	jobs := make(chan string)
	results := make(chan result, len(repos))

	var wg sync.WaitGroup

	for range min(workers, len(repos)) {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for repo := range jobs {
				names, err := a.available(ctx, repo, strings.TrimSpace(version.stdout))
				results <- result{repo, names, err}
			}
		}()
	}
	go func() {
		for _, repo := range repos {
			jobs <- repo
		}

		close(jobs)
		wg.Wait()
		close(results)
	}()

	var (
		lines        []string
		staleIgnored []string
		errs         []error
	)

	for result := range results {
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.repo, result.err))
			continue
		}

		available := make(map[string]bool, len(result.names))
		for _, name := range result.names {
			available[name] = true
			if !installed[result.repo][name] && !ignored[result.repo][name] {
				lines = append(lines, result.repo+"\t"+name)
			}
		}

		for name := range ignored[result.repo] {
			if !available[name] {
				staleIgnored = append(staleIgnored, result.repo+"\t"+name)
			}
		}
	}

	if len(errs) != 0 {
		return errors.Join(errs...)
	}

	slices.Sort(staleIgnored)

	for _, entry := range staleIgnored {
		a.warning("Ignore file entry refers to unavailable skill: " + entry)
	}

	slices.Sort(lines)

	lines = slices.Compact(lines)
	for _, line := range lines {
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return fmt.Errorf("write results: %w", err)
		}
	}

	return nil
}

func defaultIgnoreFile() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find user home directory: %w", err)
		}

		configDir = filepath.Join(home, ".config")
	}

	return filepath.Join(configDir, "skills-missing", "ignore"), nil
}

func readIgnored(path string, explicit bool) (map[string]map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return map[string]map[string]bool{}, nil
		}

		return nil, err
	}

	ignored := make(map[string]map[string]bool)

	lineNumber := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		lineNumber++

		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		repo, name, ok := strings.Cut(line, "\t")

		repo, name = strings.TrimSpace(repo), strings.TrimSpace(name)
		if !ok || strings.Contains(name, "\t") || name == "" || !repositoryPattern.MatchString(strings.ToLower(repo)) {
			return nil, fmt.Errorf("line %d: expected owner/repository<TAB>skill-name", lineNumber)
		}

		repo = strings.ToLower(repo)
		if ignored[repo] == nil {
			ignored[repo] = make(map[string]bool)
		}

		ignored[repo][name] = true
	}

	return ignored, nil
}

func (a *app) revision(ctx context.Context, repo string) string {
	out, err := a.command(ctx, "git", "ls-remote", "--exit-code", "https://github.com/"+repo+".git", "HEAD")
	if err != nil {
		a.warning(fmt.Sprintf("Could not verify %s; bypassing cache: %v", repo, err))
		return ""
	}

	matches := revisionPattern.FindStringSubmatch(strings.TrimSpace(out.stdout))
	if matches == nil {
		a.warning("Invalid remote revision for " + repo + "; bypassing cache")
		return ""
	}

	return matches[1]
}

func (a *app) available(ctx context.Context, repo, version string) ([]string, error) {
	path := filepath.Join(a.cacheDir, strings.ReplaceAll(repo, "/", "@")+".json")

	cached, cacheValid, err := a.readCache(path, repo, version)
	if err != nil {
		return nil, fmt.Errorf("read cache: %w", err)
	}

	if cacheValid && cacheIsFresh(cached.CheckedAt, a.currentTime()) {
		return cached.Names, nil
	}

	revision := a.revision(ctx, repo)
	if revision != "" && cacheValid && cached.Revision == revision {
		cached.CheckedAt = a.currentTime().UTC()
		if err := a.writeCache(path, cached); err != nil {
			a.warning(fmt.Sprintf("Could not refresh cache timestamp for %s: %v", repo, err))
		}

		return cached.Names, nil
	}

	if revision == "" {
		return nil, errors.New("cannot list skills without a verified repository revision")
	}

	if a.listRemoteSkills == nil {
		return nil, errors.New("GitHub tree skill listing is unavailable")
	}

	names, err := a.listRemoteSkills(ctx, repo, revision, a.internal == "1" || a.internal == "true")
	if err != nil {
		return nil, fmt.Errorf("list skills through the GitHub tree API: %w", err)
	}

	if names == nil {
		names = []string{}
	}

	if a.revision(ctx, repo) == revision {
		entry := cacheEntry{
			Format:    cacheFormatVersion,
			Repo:      repo,
			Revision:  revision,
			Version:   version,
			Internal:  a.internal,
			CheckedAt: a.currentTime().UTC(),
			Names:     names,
		}
		if err := a.writeCache(path, entry); err != nil {
			return nil, err
		}
	}

	return names, nil
}

func (a *app) readCache(path, repo, version string) (cacheEntry, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cacheEntry{}, false, nil
		}

		return cacheEntry{}, false, err
	}

	var cached cacheEntry
	if json.Unmarshal(data, &cached) != nil || cached.Format != cacheFormatVersion ||
		cached.Repo != repo || cached.Version != version ||
		cached.Internal != a.internal || cached.Names == nil {
		return cacheEntry{}, false, nil
	}

	return cached, true, nil
}

func (a *app) writeCache(path string, entry cacheEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}

	file, err := os.CreateTemp(a.cacheDir, ".entry-*")
	if err != nil {
		return fmt.Errorf("create cache file: %w", err)
	}
	defer os.Remove(file.Name())

	if _, err = file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write cache: %w", err)
	}

	if err = file.Close(); err != nil {
		return fmt.Errorf("close cache: %w", err)
	}

	if err = os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace cache: %w", err)
	}

	return nil
}

func (a *app) currentTime() time.Time {
	if a.now != nil {
		return a.now()
	}

	return time.Now()
}

func cacheIsFresh(checkedAt, now time.Time) bool {
	age := now.Sub(checkedAt)
	return !checkedAt.IsZero() && age >= 0 && age < cacheRevalidationInterval
}
