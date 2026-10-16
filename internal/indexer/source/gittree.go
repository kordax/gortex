package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// git object mode bits, as recorded in a tree entry.
const (
	modeTypeMask = 0o170000
	modeRegular  = 0o100000
	modeSymlink  = 0o120000
	modeGitlink  = 0o160000
)

// treeEntry is one blob of the tree plus the metadata the tree records
// for it. SymlinkTarget is filled in on first use, since it lives in
// the blob rather than in the tree.
type treeEntry struct {
	meta         FileMeta
	oid          string
	linkResolved bool
}

// GitTreeSource serves content out of a committed git tree, without a
// checkout and without touching the working directory.
//
// Enumeration is one `git ls-tree` pass, whose result is held in
// memory — one entry per blob in the tree, which is the same order of
// magnitude as the file list any indexing pass already builds. Reads go
// through a single long-lived `git cat-file` child that is spawned on
// the first Open and reused for every read afterwards. Everything is
// plumbing: no command this source runs can write to the repository,
// run a hook, or reach the network, and the child's environment
// disables the lazy fetch a partial clone would otherwise attempt on a
// missing object.
//
// The namespace is the tree's blobs. Submodule entries (gitlinks) are
// skipped, because the tree records only the commit id of another
// repository and this source cannot produce its content; directories
// are not entries at all. A symlink is reported like the filesystem
// source reports one — Symlink set, SymlinkTarget carrying the target
// text — but Open on a symlink differs: it returns the link blob (the
// target text) rather than following it, since resolving a link inside
// a tree is a decision for the caller that knows why it wants the
// bytes. FileMeta.Symlink is the flag to branch on.
type GitTreeSource struct {
	repoDir string
	treeOID string

	// order lists every path in the canonical walk order; entries maps
	// each of them to its blob. Both are fixed after construction.
	order   []string
	entries map[string]*treeEntry

	// Missing objects belong to this source generation's degraded snapshot.
	// Reconstruct the source after fetching them to recover its capability.
	missingOIDs map[string]struct{}

	// mu serializes the batch protocol and the lazy symlink-target
	// cache. The pipe carries one request and one response at a time,
	// so this lock is also what makes concurrent Open and Stat safe.
	mu     sync.Mutex
	batch  *gitBatch
	closed bool
	// spawns counts batch children started by this source. It exists so
	// a test can prove that N reads still cost one process.
	spawns int
}

// compile-time check that the source satisfies the interface.
var _ ContentSource = (*GitTreeSource)(nil)

// NewGitTreeSource opens the tree named by treeOID in the repository at
// repoDir.
//
// treeOID must be a full hexadecimal object id (SHA-1 or SHA-256); it
// is checked against that shape before git is invoked at all, so no
// caller-supplied string can ever reach a git command line as an
// option, a ref, or a revision expression. A commit id is accepted and
// peeled to its tree.
//
// If the object is absent from the local repository, or is not a tree,
// construction fails with ErrObjectMissing — the same signal a pruned
// blob gives later, and for the same reason: the source cannot serve
// content it does not have, and must not pretend the content is empty.
func NewGitTreeSource(ctx context.Context, repoDir, treeOID string) (*GitTreeSource, error) {
	abs, resolved, err := resolveGitTreeOID(ctx, repoDir, treeOID)
	if err != nil {
		return nil, err
	}

	listing, err := runGit(ctx, abs, "ls-tree", "-r", "-l", "-z", "--full-tree", resolved)
	var missingOIDs map[string]struct{}
	if err != nil {
		missing, probeErr := gitTreeHasMissingObjects(ctx, abs, resolved, true)
		switch {
		case probeErr != nil:
			return nil, fmt.Errorf("git tree source: list %s: %w (local missing-tree probe failed: %v)", resolved, err, probeErr)
		case missing:
			return nil, fmt.Errorf("git tree source: list %s: %w", resolved, ErrObjectMissing)
		}
		missingOIDs, probeErr = gitTreeMissingObjects(ctx, abs, resolved, false)
		if probeErr != nil {
			return nil, fmt.Errorf("git tree source: list %s: %w (local missing-blob probe failed: %v)", resolved, err, probeErr)
		}
		if len(missingOIDs) == 0 {
			return nil, fmt.Errorf("git tree source: list %s: %w", resolved, err)
		}
		// -l reads every blob's size. Older Git releases fail that lookup
		// for promised objects, even when lazy fetching is disabled.
		listing, err = runGit(ctx, abs, "ls-tree", "-r", "-z", "--full-tree", resolved)
		if err != nil {
			return nil, fmt.Errorf("git tree source: list %s without blob sizes: %w", resolved, err)
		}
	}
	order, entries, err := parseTreeListing(listing)
	if err != nil {
		return nil, fmt.Errorf("git tree source: list %s: %w", resolved, err)
	}
	if len(missingOIDs) != 0 {
		if err := populateLocalBlobSizes(ctx, abs, order, entries, missingOIDs); err != nil {
			return nil, fmt.Errorf("git tree source: local blob sizes for %s: %w", resolved, err)
		}
	}
	return &GitTreeSource{repoDir: abs, treeOID: resolved, order: order, entries: entries, missingOIDs: missingOIDs}, nil
}

// VerifyGitTreeObjectsLocal proves that every tree and blob reachable from
// treeOID is available in repoDir without allowing Git to lazily fetch a
// promised object. It is intended for deciding whether a degraded immutable
// generation may be rebuilt from local data.
func VerifyGitTreeObjectsLocal(ctx context.Context, repoDir, treeOID string) error {
	abs, resolved, err := resolveGitTreeOID(ctx, repoDir, treeOID)
	if err != nil {
		return err
	}
	missing, err := gitTreeHasMissingObjects(ctx, abs, resolved, false)
	if err != nil {
		return fmt.Errorf("git tree source: verify local object closure for %s: %w", resolved, err)
	}
	if missing {
		return fmt.Errorf("git tree source: object closure for %s in %s: %w", resolved, abs, ErrObjectMissing)
	}
	return nil
}

func resolveGitTreeOID(ctx context.Context, repoDir, treeOID string) (string, string, error) {
	if !oidPattern.MatchString(treeOID) {
		return "", "", fmt.Errorf("git tree source: %q is not a full hexadecimal object id", treeOID)
	}
	if strings.TrimSpace(repoDir) == "" {
		return "", "", errors.New("git tree source: empty repository directory")
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return "", "", fmt.Errorf("git tree source: resolve %s: %w", repoDir, err)
	}

	out, err := runGit(ctx, abs, "rev-parse", "--verify", "--quiet", treeOID+"^{tree}")
	if err != nil {
		if gitExitCode(err) == 1 {
			return "", "", fmt.Errorf("git tree source: %s in %s: %w", treeOID, abs, ErrObjectMissing)
		}
		missing, probeErr := gitTreeHasMissingObjects(ctx, abs, treeOID, true)
		switch {
		case probeErr != nil:
			return "", "", fmt.Errorf("git tree source: verify %s: %w (local missing-tree probe failed: %v)", treeOID, err, probeErr)
		case missing:
			return "", "", fmt.Errorf("git tree source: %s in %s: %w", treeOID, abs, ErrObjectMissing)
		default:
			return "", "", fmt.Errorf("git tree source: verify %s: %w", treeOID, err)
		}
	}
	resolved := strings.TrimSpace(string(out))
	if !oidPattern.MatchString(resolved) {
		return "", "", fmt.Errorf("git tree source: rev-parse returned %q for %s", resolved, treeOID)
	}
	return abs, resolved, nil
}

func gitTreeHasMissingObjects(ctx context.Context, repoDir, treeOID string, treesOnly bool) (bool, error) {
	missing, err := gitTreeMissingObjects(ctx, repoDir, treeOID, treesOnly)
	return len(missing) != 0, err
}

func gitTreeMissingObjects(ctx context.Context, repoDir, treeOID string, treesOnly bool) (map[string]struct{}, error) {
	args := []string{"rev-list", "--objects", "--missing=print", "--ignore-missing", "--no-object-names"}
	if treesOnly {
		args = append(args, "--filter=blob:none")
	}
	args = append(args, treeOID)
	out, err := runGit(ctx, repoDir, args...)
	if err != nil {
		return nil, err
	}
	missing := make(map[string]struct{})
	if len(bytes.TrimSpace(out)) == 0 {
		// --missing=print covers descendants; --ignore-missing also
		// lets us distinguish an absent starting object from Git errors.
		missing[treeOID] = struct{}{}
		return missing, nil
	}
	if _, err := parseMissingObjectList(out); err != nil {
		return nil, err
	}
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(line) > 0 && line[0] == '?' {
			missing[string(line[1:])] = struct{}{}
		}
	}
	return missing, nil
}

// populateLocalBlobSizes restores the size metadata after a size-free tree
// listing. Only objects proven local by the tree walk enter the batch, so a
// promised missing blob cannot terminate it or trigger a fetch.
func populateLocalBlobSizes(ctx context.Context, repoDir string, order []string, entries map[string]*treeEntry, missing map[string]struct{}) error {
	var input strings.Builder
	requested := make(map[string]struct{})
	for _, path := range order {
		oid := entries[path].oid
		if _, absent := missing[oid]; absent {
			continue
		}
		if _, duplicate := requested[oid]; duplicate {
			continue
		}
		requested[oid] = struct{}{}
		input.WriteString(oid)
		input.WriteByte('\n')
	}
	if len(requested) == 0 {
		return nil
	}
	out, err := runGitWithInput(ctx, repoDir, strings.NewReader(input.String()), "cat-file", "--batch-check")
	if err != nil {
		return err
	}
	sizes := make(map[string]int64, len(requested))
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) < 2 {
			return fmt.Errorf("git cat-file returned malformed metadata %q", line)
		}
		if _, wanted := requested[fields[0]]; !wanted {
			return fmt.Errorf("git cat-file returned unrequested object %q", fields[0])
		}
		delete(requested, fields[0])
		if len(fields) == 2 && fields[1] == "missing" {
			missing[fields[0]] = struct{}{}
			continue
		}
		if len(fields) != 3 || fields[1] != "blob" {
			return fmt.Errorf("git cat-file returned unexpected blob metadata %q", line)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("git cat-file returned invalid blob size %q", line)
		}
		sizes[fields[0]] = size
	}
	if len(requested) != 0 {
		return errors.New("git cat-file omitted requested blob metadata")
	}
	for _, entry := range entries {
		if size, ok := sizes[entry.oid]; ok {
			entry.meta.Size = size
		}
	}
	return nil
}

func parseMissingObjectList(out []byte) (bool, error) {
	missing := false
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if line[0] == '?' {
			missing = true
			line = line[1:]
		}
		if !oidPattern.Match(line) {
			return false, fmt.Errorf("git rev-list returned malformed object id %q", line)
		}
	}
	return missing, nil
}

// parseTreeListing parses `ls-tree -r -l -z` output.
//
// The -z form is what makes this safe: records are NUL-separated and
// paths are emitted raw, so a path containing a space, a newline, or
// non-ASCII bytes arrives intact instead of being quoted and needing to
// be unquoted. Each record is "<mode> <type> <oid> <size>\t<path>",
// with the size column right-aligned, "-" for a non-blob, and "BAD" for
// a blob whose bytes are gone locally — the last of which is reported
// as an unknown size rather than a zero one.
func parseTreeListing(out []byte) ([]string, map[string]*treeEntry, error) {
	entries := make(map[string]*treeEntry)
	var order []string
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, nil, fmt.Errorf("ls-tree record without a path separator: %q", rec)
		}
		fields := strings.Fields(string(rec[:tab]))
		if len(fields) < 3 {
			return nil, nil, fmt.Errorf("ls-tree record with %d fields: %q", len(fields), rec)
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("ls-tree mode %q: %w", fields[0], err)
		}
		if mode&modeTypeMask == modeGitlink {
			// A submodule boundary: the tree records another
			// repository's commit id, not content this source can read.
			continue
		}
		kind := mode & modeTypeMask
		if kind != modeRegular && kind != modeSymlink {
			// Trees, which -r already recursed through, and anything a
			// future git might add.
			continue
		}
		oid := fields[2]
		if !oidPattern.MatchString(oid) {
			return nil, nil, fmt.Errorf("ls-tree object id %q is not hexadecimal", oid)
		}
		size := int64(-1)
		if len(fields) >= 4 {
			if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
				size = n
			}
		}
		rel, err := normalizePath(string(rec[tab+1:]))
		if err != nil {
			return nil, nil, fmt.Errorf("ls-tree path %q: %w", rec[tab+1:], err)
		}
		meta := FileMeta{Path: rel, Size: size, Mode: fs.FileMode(mode & 0o777)}
		if kind == modeSymlink {
			meta.Symlink = true
			// git records no meaningful permission bits for a link.
			meta.Mode = fs.ModeSymlink | 0o777
		}
		if _, dup := entries[rel]; !dup {
			order = append(order, rel)
		}
		entries[rel] = &treeEntry{meta: meta, oid: oid}
	}
	slices.Sort(order)
	return order, entries, nil
}

// Identity returns a stable description of the source: the repository
// directory and the resolved tree object id.
func (s *GitTreeSource) Identity() string { return "git:" + s.repoDir + "@" + s.treeOID }

// Close terminates the batch child, if one was ever spawned. It is
// idempotent, and a closed source refuses further reads.
func (s *GitTreeSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.batch != nil {
		s.batch.close()
		s.batch = nil
	}
	return nil
}

// Stat reports the tree's metadata for path. For a symlink the target
// text lives in a blob, so the first Stat of a link reads it through
// the batch child — spawning that child if this is the first read of
// any kind — and caches it; later Stats of the same link are free.
func (s *GitTreeSource) Stat(p string) (FileMeta, error) {
	rel, err := normalizePath(p)
	if err != nil {
		return FileMeta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statLocked(rel)
}

// Open returns a reader over the entry's blob plus the same metadata
// Stat reports. The blob is materialized in memory; see gitBatch.read
// for why, and note that callers cap the size of what they admit.
//
// A blob the local repository no longer holds returns ErrObjectMissing.
// A path the tree does not contain — including a directory, which is
// not an entry — returns ErrNotInSource.
func (s *GitTreeSource) Open(p string) (io.ReadCloser, FileMeta, error) {
	rel, err := normalizePath(p)
	if err != nil {
		return nil, FileMeta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entryLocked(rel)
	if err != nil {
		return nil, FileMeta{}, err
	}
	data, err := s.readBlobLocked(e.oid)
	if err != nil {
		return nil, FileMeta{}, fmt.Errorf("%s: %w", rel, err)
	}
	if e.meta.Symlink && !e.linkResolved {
		e.meta.SymlinkTarget = string(data)
		e.linkResolved = true
	}
	return io.NopCloser(bytes.NewReader(data)), e.meta, nil
}

// Walk visits every blob of the tree in lexicographic path order — the
// same order the filesystem source uses, so the two can be merged.
// Submodule entries are not visited. Symlink targets are resolved as
// they are reached, which means a tree containing links spawns the
// batch child during the walk.
func (s *GitTreeSource) Walk(ctx context.Context, fn func(FileMeta) error) error {
	if fn == nil {
		return errors.New("git tree source: nil walk function")
	}
	for _, rel := range s.order {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		meta, err := s.statLocked(rel)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if err := fn(meta); err != nil {
			return err
		}
	}
	return nil
}

// statLocked resolves rel's metadata, filling a symlink's target on
// first use. The caller must hold s.mu.
func (s *GitTreeSource) statLocked(rel string) (FileMeta, error) {
	e, err := s.entryLocked(rel)
	if err != nil {
		return FileMeta{}, err
	}
	if e.meta.Symlink && !e.linkResolved {
		data, err := s.readBlobLocked(e.oid)
		if err != nil {
			return FileMeta{}, fmt.Errorf("%s: %w", rel, err)
		}
		e.meta.SymlinkTarget = string(data)
		e.linkResolved = true
	}
	return e.meta, nil
}

// entryLocked looks up rel. The caller must hold s.mu.
func (s *GitTreeSource) entryLocked(rel string) (*treeEntry, error) {
	e, ok := s.entries[rel]
	if !ok {
		return nil, fmt.Errorf("%s: %w", rel, ErrNotInSource)
	}
	return e, nil
}

// readBlobLocked reads one blob through the batch child, starting it if
// this is the first read. The caller must hold s.mu.
func (s *GitTreeSource) readBlobLocked(oid string) ([]byte, error) {
	if s.closed {
		return nil, errors.New("git tree source: closed")
	}
	if _, missing := s.missingOIDs[oid]; missing {
		return nil, fmt.Errorf("git object %s: %w", oid, ErrObjectMissing)
	}
	if s.batch == nil {
		b, err := startGitBatch(context.Background(), s.repoDir)
		if err != nil {
			return nil, err
		}
		s.batch = b
		s.spawns++
	}
	data, err := s.batch.read(oid)
	if err == nil || s.batch.broken == nil {
		return data, err
	}
	// A promised object can disappear after enumeration. Some Git releases
	// then terminate the batch instead of replying "missing". Confirm local
	// absence without interpreting stderr or scanning unrelated objects.
	out, probeErr := runGit(context.Background(), s.repoDir, "rev-parse", "--disambiguate="+oid)
	if probeErr == nil && len(bytes.TrimSpace(out)) == 0 {
		s.batch.close()
		s.batch = nil
		if s.missingOIDs == nil {
			s.missingOIDs = make(map[string]struct{})
		}
		s.missingOIDs[oid] = struct{}{}
		return nil, fmt.Errorf("git object %s: %w: %w", oid, ErrObjectMissing, err)
	}
	return nil, err
}

// spawnCount reports how many batch children this source has started.
// It backs the test that one source costs one process no matter how
// many blobs are read.
func (s *GitTreeSource) spawnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawns
}
