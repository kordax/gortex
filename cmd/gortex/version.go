package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	semver "github.com/zzet/gortex/internal/version"
)

var versionShort bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information (SemVer + commit + build metadata)",
	Long: `Prints the running build's SemVer identity in canonical form:

  vMAJOR.MINOR.PATCH[-PRERELEASE][+COMMITSHA]

With --short, only the canonical string is emitted — useful for shell
substitution and tool-version checks. Without --short, the command
emits a multi-line summary including commit SHA, build timestamp, Go
toolchain, and host os/arch for bug reports.`,
	Run: runVersion,
}

func init() {
	versionCmd.Flags().BoolVar(&versionShort, "short", false,
		"print just the canonical SemVer string (useful for scripts)")
	rootCmd.AddCommand(versionCmd)
	rootCmd.Version = canonicalVersion()
	cobra.AddTemplateFunc("gortexVersionSummary", versionSummary)
	rootCmd.SetVersionTemplate("{{gortexVersionSummary}}")
}

func runVersion(cmd *cobra.Command, _ []string) {
	v, err := semver.Compose(versionFromMain(), commitFromMain())
	if err != nil {
		// Treat injection failures the same as a dev build rather than
		// erroring out — users running `gortex version` on a broken
		// build still need the rest of the summary to report the issue.
		v = semver.Version{Build: commitFromMain()}
	}

	if versionShort {
		fmt.Fprintln(cmd.OutOrStdout(), canonical(v))
		return
	}

	fmt.Fprint(cmd.OutOrStdout(), versionSummary())
}

// versionSummary is shared by the version command and Cobra's -v/--version flags.
func versionSummary() string {
	v, err := semver.Compose(versionFromMain(), commitFromMain())
	if err != nil {
		v = semver.Version{Build: commitFromMain()}
	}
	var summary bytes.Buffer
	w := &summary
	fmt.Fprintf(w, "gortex %s (fork by kordax)\n", canonical(v))
	fmt.Fprintf(w, "  source:  %s\n", upgradeRepoURL)
	if v.IsZero() && v.Build == "" {
		fmt.Fprintln(w, "  (dev build — no ldflags injected; build via `make build` or `goreleaser` for a versioned binary)")
	}
	fmt.Fprintf(w, "  commit:  %s\n", printable(commitFromMain(), "(none)"))
	fmt.Fprintf(w, "  built:   %s\n", printable(dateFromMain(), "(unknown)"))
	fmt.Fprintf(w, "  go:      %s\n", runtime.Version())
	fmt.Fprintf(w, "  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return summary.String()
}

// canonical renders a Version with a graceful fallback for dev builds.
// A zero Version with no build slot renders "v0.0.0-dev" so scripts
// using `gortex version --short` always get a parseable-ish string
// instead of an unexpected "v0.0.0" that looks like a real release.
func canonical(v semver.Version) string {
	if v.IsZero() && v.Build == "" {
		return "v0.0.0-dev"
	}
	return v.String()
}

// versionFromMain / commitFromMain / dateFromMain are tiny wrappers so
// the version logic stays testable — tests that want to rewrite these
// for a fake build can shadow them without having to reach into main.
func versionFromMain() string { return version }
func commitFromMain() string {
	info, ok := debug.ReadBuildInfo()
	var vcsCommit string
	if ok {
		vcsCommit = vcsBuildCommit(info.Settings)
	}
	if commit != "" {
		if (vcsCommit == "dirty" || strings.HasSuffix(vcsCommit, ".dirty")) && !strings.HasSuffix(commit, ".dirty") {
			return commit + ".dirty"
		}
		return commit
	}
	return vcsCommit
}

// vcsBuildCommit preserves Go's source identity for plain go build as well as
// release builds. A dirty checkout must never look like a clean release.
func vcsBuildCommit(settings []debug.BuildSetting) string {
	var revision string
	var modified bool
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if modified {
		if revision == "" {
			return "dirty"
		}
		revision += ".dirty"
	}
	return revision
}
func dateFromMain() string { return date }

// printable picks a non-empty string or returns the provided fallback —
// used so empty injection slots render as "(none)" instead of a blank.
func printable(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// canonicalVersion returns the running binary's SemVer string in
// canonical form (e.g. "v0.1.0+abc1234"). Used by the daemon handshake
// and any other place where Gortex identifies itself to a peer — keeps
// the wire format consistent regardless of whether the caller holds
// `main.version` or reconstructs it.
func canonicalVersion() string {
	v, err := semver.Compose(versionFromMain(), commitFromMain())
	if err != nil {
		// Fall back to raw version string; better to surface the weird
		// value than to lie about it.
		return versionFromMain()
	}
	return canonical(v)
}

// --- gortex version bump <kind> -------------------------------------------

var versionBumpPrerelease string

var versionBumpCmd = &cobra.Command{
	Use:   "bump <major|minor|patch>",
	Short: "Bump the version in cmd/gortex/main.go and print the tag-release command",
	Long: `Rewrites main.version in cmd/gortex/main.go — the single source of truth
— by incrementing the requested part. After the file is updated, the
output suggests the git commit + tag commands to finish the release.

Must be run from the repo root (where cmd/gortex/main.go lives); the
bump is a source-file edit, not a git operation, so the command
refuses to run without that file in place.

Bump rules:
  major    MAJOR++, MINOR and PATCH → 0, prerelease cleared
  minor    MINOR++, PATCH → 0, prerelease cleared
  patch    PATCH++, prerelease cleared

--pre adds or replaces a prerelease identifier (e.g. --pre rc.1). Use
it to mark a release candidate: "gortex version bump minor --pre rc.1"
goes from v0.1.0 to v0.2.0-rc.1.`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"major", "minor", "patch"},
	RunE:      runVersionBump,
}

func init() {
	versionBumpCmd.Flags().StringVar(&versionBumpPrerelease, "pre", "",
		"attach a prerelease identifier (e.g. rc.1, alpha.2); overrides any existing one")
	versionCmd.AddCommand(versionBumpCmd)
}

// mainGoVersionRe matches the `version = "..."` declaration in main.go,
// whether it lives inside a var (…) block (leading tab / spaces only) or
// on a top-level `var version = "..."` line (leading "var "). Capture 2
// is the version string itself without its surrounding quotes.
//
// Writing this as a single regex keeps the rewriter simple; the var-
// block form is what Gortex actually ships, but the plain form shows up
// in test fixtures and third-party forks.
var mainGoVersionRe = regexp.MustCompile(`(?m)^(\s*(?:var\s+)?version\s*=\s*)"([^"]*)"(.*)$`)

func runVersionBump(cmd *cobra.Command, args []string) error {
	kind := args[0]
	switch kind {
	case "major", "minor", "patch":
		// ok
	default:
		return fmt.Errorf("bump kind must be major, minor, or patch (got %q)", kind)
	}

	// Locate cmd/gortex/main.go relative to the current working
	// directory. Refuse to run outside the repo — this is a dev tool.
	path := filepath.Join("cmd", "gortex", "main.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w (run this from the repo root)", path, err)
	}

	m := mainGoVersionRe.FindSubmatch(raw)
	if m == nil {
		return fmt.Errorf(`could not find a "version = \"...\"" line in %s`, path)
	}
	current := string(m[2])

	next, err := bumpSemver(current, kind, versionBumpPrerelease)
	if err != nil {
		return err
	}

	// Rewrite using FindSubmatchIndex so we only touch the quoted value.
	idx := mainGoVersionRe.FindSubmatchIndex(raw)
	// idx[4]/idx[5] are the start/end of the 2nd capture group (the
	// version string) — surgical replace avoids accidentally rewriting
	// the comment on the same line.
	var buf bytes.Buffer
	buf.Write(raw[:idx[4]])
	buf.WriteString(next.String()[1:]) // drop leading 'v' — main.go stores the raw semver
	buf.Write(raw[idx[5]:])

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Bumped %s: %s → %s\n", kind, current, next.String()[1:])
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next steps:")
	fmt.Fprintf(w, "  git add %s\n", path)
	fmt.Fprintf(w, "  git commit -m \"Bump version to %s\"\n", next.String())
	fmt.Fprintf(w, "  git tag %s\n", next.String())
	fmt.Fprintf(w, "  git push && git push origin %s\n", next.String())
	return nil
}

// bumpSemver applies the bump rule to a SemVer string and returns the
// resulting Version. Pre-release (if supplied) overrides whatever was
// in the source. Build slot is always cleared — it gets filled in at
// build time by the ldflag injection, not checked into main.go.
func bumpSemver(current, kind, pre string) (semver.Version, error) {
	v, err := semver.Parse("v" + strings.TrimPrefix(current, "v"))
	if err != nil {
		return semver.Version{}, fmt.Errorf("main.version %q is not valid semver: %w", current, err)
	}
	switch kind {
	case "major":
		v.Major++
		v.Minor = 0
		v.Patch = 0
	case "minor":
		v.Minor++
		v.Patch = 0
	case "patch":
		v.Patch++
	}
	v.Prerelease = pre
	v.Build = ""
	return v, nil
}
