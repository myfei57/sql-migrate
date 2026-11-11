package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/rubenv/sql-migrate/sqlparse"
)

const (
	// defaultMaxIncludeDepth bounds the IncludeFragment call chain depth.
	defaultMaxIncludeDepth = 16
	// defaultMaxIncludeBytes bounds the cumulative fragment bytes materialized
	// while expanding a single root migration.
	defaultMaxIncludeBytes int64 = 10 << 20
	// fragmentFileSuffix is the mandatory suffix of reusable fragment files.
	fragmentFileSuffix = ".sqlinc"
)

// FragmentFileSystemMigrationSource loads .sql migrations from an explicit
// fs.FS and expands optional '-- +migrate IncludeFragment' annotations using
// .sqlinc fragments living in the same file system.
//
// Fragments never become migrations on their own: they produce no
// MigrationRecord rows and can only be pulled into the Up/Down sections of a
// root .sql migration. The fragment namespace is resolved from the declared
// FragmentRoot, so annotations can never reach arbitrary host paths or
// environment variables.
type FragmentFileSystemMigrationSource struct {
	// FileSystem is the explicit file system holding migrations and fragments.
	FileSystem fs.FS
	// Root is the directory containing the root .sql migration files.
	Root string
	// FragmentRoot is the directory against which IncludeFragment paths are
	// resolved. It defaults to Root.
	FragmentRoot string
	// MaxIncludeDepth bounds the IncludeFragment call chain depth. Zero means
	// the built-in default is used.
	MaxIncludeDepth int
	// MaxIncludeBytes bounds the cumulative fragment bytes materialized per
	// root migration expansion. Zero means the built-in default is used.
	MaxIncludeBytes int64
}

var _ MigrationSource = (*FragmentFileSystemMigrationSource)(nil)

// SnapshotChangedError reports that a source file changed between reads during
// one load. The whole load is rejected so that two file generations are never
// mixed.
type SnapshotChangedError struct {
	// Path is the file whose content changed.
	Path string
}

func (e *SnapshotChangedError) Error() string {
	return fmt.Sprintf("migration source file %q changed while loading; refusing to mix file generations", e.Path)
}

type cachedFile struct {
	data   []byte
	digest string
}

// fsSnapshot freezes one fs.FS cut-point for a whole load: the first read of a
// file is cached and every later read is re-validated against the underlying
// file system.
type fsSnapshot struct {
	fsys  fs.FS
	files map[string]*cachedFile
	dirs  map[string][]fs.DirEntry
}

func newFSSnapshot(fsys fs.FS) *fsSnapshot {
	return &fsSnapshot{
		fsys:  fsys,
		files: make(map[string]*cachedFile),
		dirs:  make(map[string][]fs.DirEntry),
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *fsSnapshot) readFile(name string) ([]byte, string, error) {
	if cached, ok := s.files[name]; ok {
		fresh, err := fs.ReadFile(s.fsys, name)
		if err != nil {
			return nil, "", fmt.Errorf("re-reading %s: %w", name, err)
		}
		if digestBytes(fresh) != cached.digest {
			return nil, "", &SnapshotChangedError{Path: name}
		}
		return cached.data, cached.digest, nil
	}

	data, err := fs.ReadFile(s.fsys, name)
	if err != nil {
		return nil, "", err
	}
	digest := digestBytes(data)
	s.files[name] = &cachedFile{data: data, digest: digest}
	return data, digest, nil
}

func (s *fsSnapshot) readDir(name string) ([]fs.DirEntry, error) {
	if cached, ok := s.dirs[name]; ok {
		return cached, nil
	}
	entries, err := fs.ReadDir(s.fsys, name)
	if err != nil {
		return nil, err
	}
	s.dirs[name] = entries
	return entries, nil
}

// verify re-validates every cached file against the underlying file system and
// rejects the load if any file drifted after its first read.
func (s *fsSnapshot) verify() error {
	names := make([]string, 0, len(s.files))
	for name := range s.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fresh, err := fs.ReadFile(s.fsys, name)
		if err != nil {
			return fmt.Errorf("re-reading %s: %w", name, err)
		}
		if digestBytes(fresh) != s.files[name].digest {
			return &SnapshotChangedError{Path: name}
		}
	}
	return nil
}

// fragmentLoader resolves IncludeFragment references against the fixed
// snapshot and enforces the namespace, depth, cycle and cumulative byte
// limits. A fresh loader is created per root migration so per-migration
// accounting never leaks between migrations.
type fragmentLoader struct {
	snap         *fsSnapshot
	fragmentRoot string
	maxDepth     int
	maxBytes     int64
	bytesRead    int64
}

var _ sqlparse.FragmentResolver = (*fragmentLoader)(nil)

func (l *fragmentLoader) Resolve(ref string, site sqlparse.SourceRef, chain []sqlparse.IncludeFrame) (sqlparse.Fragment, error) {
	resolved, err := resolveFragmentPath(l.fragmentRoot, ref)
	if err != nil {
		return sqlparse.Fragment{}, err
	}

	for _, frame := range chain {
		if frame.Path == resolved {
			return sqlparse.Fragment{}, fmt.Errorf("include cycle detected at %s:%d: %s is already part of the include chain %s",
				site.File, site.Line, resolved, formatIncludeChain(chain, resolved))
		}
	}
	if len(chain)+1 > l.maxDepth {
		return sqlparse.Fragment{}, fmt.Errorf("include depth of %d exceeds the limit of %d at %s:%d",
			len(chain)+1, l.maxDepth, site.File, site.Line)
	}

	info, err := fs.Stat(l.snap.fsys, resolved)
	if err != nil {
		return sqlparse.Fragment{}, fmt.Errorf("fragment %q: %w", ref, err)
	}
	if info.IsDir() {
		return sqlparse.Fragment{}, fmt.Errorf("fragment %q is a directory, not a file", ref)
	}

	data, digest, err := l.snap.readFile(resolved)
	if err != nil {
		return sqlparse.Fragment{}, err
	}

	l.bytesRead += int64(len(data))
	if l.bytesRead > l.maxBytes {
		return sqlparse.Fragment{}, fmt.Errorf("cumulative fragment size of %d bytes exceeds the limit of %d at %s:%d",
			l.bytesRead, l.maxBytes, site.File, site.Line)
	}

	return sqlparse.Fragment{
		Path:    resolved,
		Content: data,
		Digest:  digest,
	}, nil
}

func formatIncludeChain(chain []sqlparse.IncludeFrame, closing string) string {
	parts := make([]string, 0, len(chain)+1)
	for _, frame := range chain {
		parts = append(parts, frame.Path)
	}
	parts = append(parts, closing)
	return strings.Join(parts, " -> ")
}

// resolveFragmentPath maps an authored fragment reference to a cleaned path
// contained in the declared fragment root. Absolute paths, host separators and
// any reference escaping the root are rejected.
func resolveFragmentPath(root, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("include fragment path must not be empty")
	}
	if strings.ContainsAny(ref, `\`) {
		return "", fmt.Errorf("include fragment path %q must use slash separators", ref)
	}
	if path.IsAbs(ref) {
		return "", fmt.Errorf("include fragment path %q must not be absolute", ref)
	}

	cleanRoot := path.Clean(root)
	resolved := path.Clean(path.Join(cleanRoot, ref))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("include fragment path %q escapes the fragment root %q", ref, cleanRoot)
	}
	if cleanRoot != "." && resolved != cleanRoot && !strings.HasPrefix(resolved, cleanRoot+"/") {
		return "", fmt.Errorf("include fragment path %q escapes the fragment root %q", ref, cleanRoot)
	}
	if !fs.ValidPath(resolved) {
		return "", fmt.Errorf("include fragment path %q is not a valid fs.FS path", ref)
	}
	if !strings.HasSuffix(resolved, fragmentFileSuffix) {
		return "", fmt.Errorf("include fragment path %q must have the %s suffix", ref, fragmentFileSuffix)
	}
	return resolved, nil
}

// FindMigrations implements MigrationSource.
func (f *FragmentFileSystemMigrationSource) FindMigrations() ([]*Migration, error) {
	if f.FileSystem == nil {
		return nil, fmt.Errorf("FragmentFileSystemMigrationSource.FileSystem must not be nil")
	}

	root := path.Clean(f.Root)
	if root == "" || root == "/" {
		root = "."
	}
	fragmentRoot := path.Clean(f.FragmentRoot)
	if fragmentRoot == "" || fragmentRoot == "/" {
		fragmentRoot = root
	}
	maxDepth := f.MaxIncludeDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxIncludeDepth
	}
	maxBytes := f.MaxIncludeBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxIncludeBytes
	}

	snapshot := newFSSnapshot(f.FileSystem)
	entries, err := snapshot.readDir(root)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	includeMarker := []byte("-- +migrate " + sqlparse.IncludeFragmentCommand)
	migrations := make([]*Migration, 0, len(names))
	for _, name := range names {
		fullPath := path.Join(root, name)
		data, _, err := snapshot.readFile(fullPath)
		if err != nil {
			return nil, err
		}

		loader := &fragmentLoader{
			snap:         snapshot,
			fragmentRoot: fragmentRoot,
			maxDepth:     maxDepth,
			maxBytes:     maxBytes,
		}

		var parsed *sqlparse.ParsedMigration
		if bytes.Contains(data, includeMarker) {
			parsed, err = sqlparse.ParseMigrationWithIncludes(bytes.NewReader(data), sqlparse.IncludeConfig{
				Resolver: loader,
				File:     name,
			})
		} else {
			// Files without any IncludeFragment annotation go through the
			// untouched legacy parser, keeping historical byte output.
			parsed, err = sqlparse.ParseMigration(bytes.NewReader(data))
		}
		if err != nil {
			return nil, fmt.Errorf("Error while parsing %s: %w", name, err)
		}

		migrations = append(migrations, &Migration{
			Id:                     name,
			Up:                     parsed.UpStatements,
			Down:                   parsed.DownStatements,
			DisableTransactionUp:   parsed.DisableTransactionUp,
			DisableTransactionDown: parsed.DisableTransactionDown,
			UpSources:              parsed.UpSources,
			DownSources:            parsed.DownSources,
		})
	}

	// Reject the whole load when any cached file drifted: no statement from a
	// mixed generation is ever handed to the executor.
	if err := snapshot.verify(); err != nil {
		return nil, err
	}

	sort.Sort(byId(migrations))
	return migrations, nil
}

// AsSnapshotChangedError reports whether err is a SnapshotChangedError.
func AsSnapshotChangedError(err error) bool {
	var changed *SnapshotChangedError
	return errors.As(err, &changed)
}
