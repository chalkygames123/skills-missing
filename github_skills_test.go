package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubAPIFailureDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		headers  http.Header
		token    string
		want     []string
		unwanted []string
	}{
		{
			name: "primary rate limit", status: http.StatusForbidden,
			headers: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1700000000"}},
			want:    []string{"403 Forbidden", "no token provided", "API rate limit remaining: 0", "2023-11-14T22:13:20Z"},
		},
		{
			name: "other forbidden", status: http.StatusForbidden, token: "secret-token",
			want: []string{"403 Forbidden", "token provided"}, unwanted: []string{"rate limit", "secret-token"},
		},
		{
			name: "forbidden with retry", status: http.StatusForbidden,
			headers: http.Header{"Retry-After": {"30"}},
			want:    []string{"403 Forbidden", "retry after 30 seconds"}, unwanted: []string{"rate limit"},
		},
		{
			name: "retry", status: http.StatusTooManyRequests,
			headers: http.Header{"Retry-After": {"60"}},
			want:    []string{"429 Too Many Requests", "API rate limited", "retry after 60 seconds"},
		},
		{
			name: "unauthorized", status: http.StatusUnauthorized, token: "secret-token",
			want: []string{"401 Unauthorized", "authentication required or rejected"}, unwanted: []string{"secret-token"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for key, values := range tt.headers {
					w.Header()[key] = values
				}

				http.Error(w, "secret response body", tt.status)
			}))
			defer server.Close()

			lister := githubTreeLister{client: server.Client(), token: tt.token}

			_, err := lister.get(t.Context(), server.URL, true, maxGitHubTreeSize)
			if err == nil {
				t.Fatal("expected API error")
			}

			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}

			for _, unwanted := range append(tt.unwanted, "secret response body") {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("error %q contains %q", err, unwanted)
				}
			}
		})
	}
}

func TestParseSkillDocumentNameYAMLCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name     string
		document string
		wantName string
		internal bool
		wantErr  bool
	}{
		{
			name:     "folded description and boolean metadata",
			document: "---\nname: example\ndescription: >-\n  A useful skill.\nmetadata:\n  internal: true\n---\n",
			wantName: "example", internal: true,
		},
		{
			name:     "quoted internal metadata",
			document: "---\nname: example\ndescription: Useful.\nmetadata:\n  internal: \"1\"\n---\n",
			wantName: "example", internal: true,
		},
		{
			name:     "non-string name",
			document: "---\nname: 42\ndescription: Useful.\n---\n",
			wantErr:  true,
		},
		{
			name:     "duplicate name",
			document: "---\nname: first\nname: second\ndescription: Useful.\n---\n",
			wantErr:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name, internal, err := parseSkillDocumentName([]byte(tt.document))
			if (err != nil) != tt.wantErr || name != tt.wantName || internal != tt.internal {
				t.Fatalf("name %q, internal %t, error %v", name, internal, err)
			}
		})
	}
}

func TestOfflineAPIProjectionWithSkillsCLI(t *testing.T) {
	if _, err := exec.LookPath("skills"); err != nil {
		t.Skip("skills CLI is not installed")
	}

	t.Setenv("HOME", t.TempDir())

	revision := strings.Repeat("c", 40)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/repo/git/trees/" + revision:
			fmt.Fprint(w, `{"tree":[
				{"path":"skills/available/SKILL.md","type":"blob"},
				{"path":"docs/nested/SKILL.md","type":"blob"},
				{"path":".github/skills/hidden/SKILL.md","type":"blob"}
			]}`)
		case "/example/repo/" + revision + "/skills/available/SKILL.md":
			fmt.Fprint(w, "---\nname: available\ndescription: Available.\n---\n")
		case "/example/repo/" + revision + "/docs/nested/SKILL.md":
			fmt.Fprint(w, "---\nname: nested\ndescription: Nested.\n---\n")
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lister := githubTreeLister{client: server.Client(), apiBaseURL: server.URL, rawBaseURL: server.URL}

	names, err := lister.listWithFallback(t.Context(), "example/repo", revision, false)
	if err != nil || strings.Join(names, ",") != "available" {
		t.Fatalf("names %v, error %v", names, err)
	}
}

func TestGitHubTreeListerFetchesAndFiltersSkillDocuments(t *testing.T) {
	revision := strings.Repeat("a", 40)

	var rawRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header %q", r.Header.Get("Authorization"))
		}

		if r.URL.Path == "/repos/example/repo/git/trees/"+revision {
			if r.URL.Query().Get("recursive") != "1" {
				t.Errorf("recursive query %q", r.URL.RawQuery)
			}

			fmt.Fprint(w, `{"tree":[
				{"path":"skills/first/SKILL.md","type":"blob"},
				{"path":"skills/internal/SKILL.md","type":"blob"},
				{"path":".github/skills/hidden/SKILL.md","type":"blob"},
				{"path":"skills/.curated/hidden/SKILL.md","type":"blob"},
				{"path":"skills/first/.nested/SKILL.md","type":"blob"},
				{"path":"node_modules/ignored/SKILL.md","type":"blob"}
			]}`)

			return
		}

		rawRequests.Add(1)

		switch r.URL.Path {
		case "/example/repo/" + revision + "/skills/first/SKILL.md":
			fmt.Fprint(w, "---\nname: renamed-skill\ndescription: >-\n  A valid description.\n---\n")
		case "/example/repo/" + revision + "/skills/internal/SKILL.md":
			fmt.Fprint(w, "---\nname: internal-skill\ndescription: Internal.\nmetadata:\n  internal: true\n---\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lister := githubTreeLister{
		client:     server.Client(),
		apiBaseURL: server.URL,
		rawBaseURL: server.URL,
		token:      "test-token",
	}

	names, err := lister.list(t.Context(), "example/repo", revision, false)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := strings.Join(names, ","), "renamed-skill"; got != want {
		t.Fatalf("names %q, want %q", got, want)
	}

	if got := rawRequests.Load(); got != 2 {
		t.Fatalf("read %d raw documents, want 2", got)
	}

	names, err = lister.list(t.Context(), "example/repo", revision, true)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := strings.Join(names, ","), "internal-skill,renamed-skill"; got != want {
		t.Fatalf("including internal skills: got %q, want %q", got, want)
	}

	if got := rawRequests.Load(); got != 4 {
		t.Fatalf("read %d raw documents across both calls, want 4", got)
	}
}

func TestGitHubTreeListerReturnsEmptyForHiddenOnlySkills(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tree":[
			{"path":".github/skills/example/SKILL.md","type":"blob"},
			{"path":"skills/.curated/example/SKILL.md","type":"blob"},
			{"path":".hidden/submodule","type":"commit","sha":"abc"}
		]}`)
	}))
	defer server.Close()

	lister := githubTreeLister{client: server.Client(), apiBaseURL: server.URL, rawBaseURL: server.URL}

	names, err := lister.list(t.Context(), "example/repo", strings.Repeat("a", 40), false)
	if err != nil || len(names) != 0 {
		t.Fatalf("names %v, error %v", names, err)
	}
}

func TestGitHubTreeListerUsesOnlyRootSkillWhenPresent(t *testing.T) {
	revision := strings.Repeat("b", 40)

	var rawRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example/repo/git/trees/"+revision {
			fmt.Fprint(w, `{"tree":[
				{"path":"SKILL.md","type":"blob"},
				{"path":"skills/other/SKILL.md","type":"blob"}
			]}`)

			return
		}

		rawRequests.Add(1)
		fmt.Fprint(w, "---\nname: root-skill\ndescription: Root skill.\n---\n")
	}))
	defer server.Close()

	lister := githubTreeLister{client: server.Client(), apiBaseURL: server.URL, rawBaseURL: server.URL}

	names, err := lister.list(t.Context(), "example/repo", revision, false)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := strings.Join(names, ","), "root-skill"; got != want {
		t.Fatalf("names %q, want %q", got, want)
	}

	if got := rawRequests.Load(); got != 1 {
		t.Fatalf("read %d raw documents, want only the root skill", got)
	}
}

func TestGitHubTreeListerProjectsNestedSkills(t *testing.T) {
	revision := strings.Repeat("c", 40)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example/repo/git/trees/"+revision {
			fmt.Fprint(w, `{"tree":[
				{"path":"plugins/example/skills/nested/SKILL.md","type":"blob"},
				{"path":".github/skills/hidden/SKILL.md","type":"blob"},
				{"path":".claude-plugin/marketplace.json","type":"blob"}
			]}`)

			return
		}

		switch r.URL.Path {
		case "/example/repo/" + revision + "/plugins/example/skills/nested/SKILL.md":
			fmt.Fprint(w, "---\nname: nested\ndescription: Visible.\n---\n")
		case "/example/repo/" + revision + "/.claude-plugin/marketplace.json":
			fmt.Fprint(w, `{"plugins":[{"source":"./plugins/example"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lister := githubTreeLister{
		client: server.Client(), apiBaseURL: server.URL, rawBaseURL: server.URL,
		listSkills: func(_ context.Context, dir string, internal bool) (output, error) {
			for _, name := range []string{"plugins/example/skills/nested/SKILL.md", ".claude-plugin/marketplace.json"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("missing projected %s: %v", name, err)
				}
			}

			if _, err := os.Stat(filepath.Join(dir, ".github/skills/hidden/SKILL.md")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("hidden skill was projected: %v", err)
			}

			if internal {
				t.Error("internal skills unexpectedly enabled")
			}

			return output{stdout: "◇  Available Skills\n│    nested\n└  Use --skill <name> to install specific skills\n"}, nil
		},
	}

	names, err := lister.list(t.Context(), "example/repo", revision, false)
	if err != nil || strings.Join(names, ",") != "nested" {
		t.Fatalf("names %v, error %v", names, err)
	}
}

func TestGitHubTreeListerSkipsSubmoduleWithoutSkills(t *testing.T) {
	revision := strings.Repeat("f", 40)
	submoduleRevision := strings.Repeat("1", 40)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/repo/git/trees/" + revision:
			fmt.Fprint(w, `{"tree":[
				{"path":".gitmodules","type":"blob"},
				{"path":"vendor/large","type":"commit","sha":"`+submoduleRevision+`"},
				{"path":"skills/available/SKILL.md","type":"blob"}
			]}`)
		case "/example/repo/" + revision + "/.gitmodules":
			fmt.Fprint(w, "[submodule \"large\"]\n\tpath = vendor/large\n\turl = https://github.com/example/large.git\n")
		case "/repos/example/large/git/trees/" + submoduleRevision:
			fmt.Fprint(w, `{"tree":[{"path":"README.md","type":"blob"}]}`)
		case "/example/repo/" + revision + "/skills/available/SKILL.md":
			fmt.Fprint(w, "---\nname: available\ndescription: Available skill.\n---\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lister := githubTreeLister{client: server.Client(), apiBaseURL: server.URL, rawBaseURL: server.URL}

	names, err := lister.list(t.Context(), "example/repo", revision, false)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := strings.Join(names, ","), "available"; got != want {
		t.Fatalf("names %q, want %q", got, want)
	}
}

func TestGitHubTreeListerRejectsSkillsInsideSubmodules(t *testing.T) {
	revision := strings.Repeat("2", 40)
	submoduleRevision := strings.Repeat("3", 40)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/repo/git/trees/" + revision:
			fmt.Fprint(w, `{"tree":[
				{"path":".gitmodules","type":"blob"},
				{"path":"vendor/skills","type":"commit","sha":"`+submoduleRevision+`"}
			]}`)
		case "/example/repo/" + revision + "/.gitmodules":
			fmt.Fprint(w, "[submodule \"skills\"]\n\tpath = vendor/skills\n\turl = ../skills.git\n")
		case "/repos/example/skills/git/trees/" + submoduleRevision:
			fmt.Fprint(w, `{"tree":[{"path":"skills/example/SKILL.md","type":"blob"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lister := githubTreeLister{client: server.Client(), apiBaseURL: server.URL, rawBaseURL: server.URL}
	if _, err := lister.list(t.Context(), "example/repo", revision, false); err == nil ||
		!strings.Contains(err.Error(), "contains skill documents") {
		t.Fatalf("expected submodule fallback error, got %v", err)
	}
}

func TestResolveSubmoduleRepo(t *testing.T) {
	repo, err := resolveSubmoduleRepo("example/parent", "../child.git")
	if err != nil {
		t.Fatal(err)
	}

	if repo != "example/child" {
		t.Fatalf("repo %q, want example/child", repo)
	}
}

func TestAvailablePrefersGitHubTreeAndCachesResult(t *testing.T) {
	cacheDir := t.TempDir()
	revision := strings.Repeat("d", 40)
	listerCalls := 0
	runner := func(_ context.Context, name string, _ ...string) (output, error) {
		if name != "git" {
			return output{}, fmt.Errorf("unexpected command %s", name)
		}

		return output{stdout: revision + "\tHEAD\n"}, nil
	}
	a := app{
		run:      runner,
		cacheDir: cacheDir,
		warn:     &strings.Builder{},
		listRemoteSkills: func(_ context.Context, repo, gotRevision string, includeInternal bool) ([]string, error) {
			listerCalls++

			if repo != "example/repo" || gotRevision != revision || includeInternal {
				t.Fatalf("unexpected list request: %s %s includeInternal=%t", repo, gotRevision, includeInternal)
			}

			return []string{"first", "second"}, nil
		},
	}

	for range 2 {
		names, err := a.available(t.Context(), "example/repo", "1.0.0")
		if err != nil {
			t.Fatal(err)
		}

		if got, want := strings.Join(names, ","), "first,second"; got != want {
			t.Fatalf("names %q, want %q", got, want)
		}
	}

	if listerCalls != 1 {
		t.Fatalf("tree lookups %d, want 1", listerCalls)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, "example@repo.json")); err != nil {
		t.Fatalf("cache file: %v", err)
	}
}

func TestAvailableFailsWhenTreeLookupFails(t *testing.T) {
	revision := strings.Repeat("e", 40)

	var warnings strings.Builder

	runner := func(_ context.Context, name string, args ...string) (output, error) {
		if name == "git" {
			return output{stdout: revision + "\tHEAD\n"}, nil
		}

		return output{}, fmt.Errorf("unexpected command: %s %v", name, args)
	}

	a := app{
		run:      runner,
		cacheDir: t.TempDir(),
		warn:     &warnings,
		listRemoteSkills: func(context.Context, string, string, bool) ([]string, error) {
			return nil, errors.New("API unavailable")
		},
	}

	names, err := a.available(t.Context(), "example/repo", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "API unavailable") || len(names) != 0 {
		t.Fatalf("names %v, error %v", names, err)
	}
}

func TestAvailableInvalidatesOldCacheAndCachesEmptyResult(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	revision := strings.Repeat("e", 40)

	old := fmt.Sprintf(`{"repo":"example/repo","revision":%q,"version":"1.0.0","checkedAt":%q,"names":["hidden"]}`,
		revision, now.Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, "example@repo.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0

	a := app{
		run: func(context.Context, string, ...string) (output, error) {
			return output{stdout: revision + "\tHEAD\n"}, nil
		},
		listRemoteSkills: func(context.Context, string, string, bool) ([]string, error) {
			calls++
			return []string{}, nil
		},
		cacheDir: dir,
		warn:     &strings.Builder{},
		now:      func() time.Time { return now },
	}
	for range 2 {
		names, err := a.available(t.Context(), "example/repo", "1.0.0")
		if err != nil || len(names) != 0 {
			t.Fatalf("names %v, error %v", names, err)
		}
	}

	if calls != 1 {
		t.Fatalf("tree lookups %d, want 1", calls)
	}
}
