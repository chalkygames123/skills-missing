# skills-missing

List skills available from GitHub repositories but not installed from those repositories with the [skills CLI](https://github.com/vercel-labs/skills).

## Requirements

- The `skills` CLI and Git available on `PATH`
- An internet connection

The `skills` CLI requires Node.js. Install it with pnpm:

```sh
pnpm add --global skills
```

The integration tests use `skills` 1.9.0.

## Installation

Download the archive for your operating system and architecture from [GitHub Releases](https://github.com/chalkygames123/skills-missing/releases), extract it, and place the executable in a directory on your `PATH`.

Alternatively, install with Go 1.27.4 or later:

```sh
go install github.com/chalkygames123/skills-missing@latest
```

Ensure Go's binary installation directory (`GOBIN`, or `GOPATH/bin` when `GOBIN` is unset) is on your `PATH`.

## Usage

```sh
skills-missing [--ignore-file PATH] [owner/repository]
```

With no argument, the command discovers GitHub source repositories from installed project and global skills. It compares each repository and original skill name pair and writes missing skills as sorted, headerless TSV records. It queries up to four repositories concurrently and writes no partial results if a lookup fails.

The default ignore file is `$XDG_CONFIG_HOME/skills-missing/ignore`, or `~/.config/skills-missing/ignore` when `XDG_CONFIG_HOME` is unset. Use `--ignore-file PATH` to select another file; flags must precede the optional repository argument. A missing default ignore file is treated as empty, while a missing explicitly selected file is an error.

Each non-comment, non-blank line in the ignore file contains a source repository and exact skill name separated by one tab: `owner/repository<TAB>skill-name`. Repository names are normalized to lowercase, surrounding whitespace is ignored, and malformed entries report their line number. An entry ignores a skill only from its specified source repository. After fetching a repository's available skills, the command warns on standard error when an ignore entry names a skill that is not listed. Only repositories queried during that run are checked; these warnings do not affect TSV output or the exit status.

```text
# Available but intentionally not installed
owner/repository	experimental-skill
```

The command uses `skills list --json` to find installed skills. On a cache miss, it uses the GitHub Trees API to find `SKILL.md` files. For complex layouts, it uses `skills add --list` to follow the CLI's discovery rules. `SKILL.md` files inside any dot-prefixed directory, including `.github/skills` and `skills/.curated`, are excluded. A repository with no remaining skills produces no records. Set `GITHUB_TOKEN` or `GH_TOKEN` to use authenticated API limits.

If the GitHub API cannot provide a complete listing, the command reports the reason on standard error, fetches the pinned Git revision and visible submodules into a temporary directory, and applies the same discovery rules. For HTTP 401, 403, and 429 responses from the API, the diagnostic indicates whether a token was provided. It also reports the reset time, if provided, when the response shows no remaining requests, and the retry interval when it is given as a number of seconds. This fallback may be slow or require significant disk space. Submodule URLs must use HTTPS and cannot contain credentials; Git system and global configuration and credential helpers are disabled for the fallback. If the source cannot be inspected completely, the lookup fails rather than returning an unfiltered listing. Temporary files are removed after the lookup. Skills without a recorded GitHub source are not included in the comparison.

Successful source listings are cached under `$XDG_CACHE_HOME/skills-missing`, or `~/.cache/skills-missing` when `XDG_CACHE_HOME` is unset, provided the repository HEAD can be verified and remains unchanged during the lookup. A cached repository HEAD is revalidated at most every five minutes, so remote skill changes may take up to five minutes to appear. The cache is also invalidated when the skills CLI version or `INSTALL_INTERNAL_SKILLS` changes.

## Development

Run `go test -race ./...` and `go vet ./...` to check changes. Tests use local fixtures rather than GitHub; tests requiring the `skills` CLI are skipped when it is unavailable. To run them with the same CLI version as CI, install the development dependencies with `pnpm ci` and add `node_modules/.bin` to `PATH`. CI tests Linux, macOS, and Windows and checks Go formatting.

Run `go tool golangci-lint run` to check whitespace, or `go tool golangci-lint run --fix` to apply suggested fixes.

Pushing a version tag such as `v0.1.0` runs the tests and publishes archives and checksums to GitHub Releases.
