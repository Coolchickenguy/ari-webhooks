package filehours

import (
	"path"
	"sort"
	"strings"
)

// FileHours is one file Hackatime credited coding time to, resolved against the
// ship's repository. These rows feed the review screen's file browser, so they
// carry every file, whatever its size or kind.
type FileHours struct {
	// Path is the repository path in its original casing (git needs it back
	// verbatim to read the file) when Status is head or history, and the last few
	// segments of the maker's local path otherwise, so a maker's home directory
	// never reaches an organizer's screen.
	Path    string
	Seconds float64
	// Bytes is the file's size on the default branch; 0 unless Status is head,
	// negative when the capture listed the file without its size.
	Bytes int64
	// Status is how the repository accounts for the file: "head" (on the default
	// branch now, source viewable), "history" (the captured commits touched it but
	// it is gone from the default branch), "none" (never seen in this repository).
	Status string
}

// pathIndex resolves an entity path to the repository path it names, keeping the
// original casing. Three readings, strictest first (full path, repository path
// as a tail of the entity, base name); ties break deterministically toward the
// shortest, then smallest, path so a re-capture never flips which repo path a
// heartbeat lands on.
type pathIndex struct {
	byPath map[string]string // normalized full path -> original
	byBase map[string]string // base name -> original
}

func newPathIndex(originals []string) *pathIndex {
	ordered := append([]string{}, originals...)
	sort.Slice(ordered, func(a, b int) bool {
		if len(ordered[a]) != len(ordered[b]) {
			return len(ordered[a]) < len(ordered[b])
		}
		return ordered[a] < ordered[b]
	})
	idx := &pathIndex{byPath: map[string]string{}, byBase: map[string]string{}}
	for _, raw := range ordered {
		normalized := normalize(raw)
		if normalized == "" || normalized == "." {
			continue
		}
		if _, taken := idx.byPath[normalized]; !taken {
			idx.byPath[normalized] = raw
		}
		if _, taken := idx.byBase[path.Base(normalized)]; !taken {
			idx.byBase[path.Base(normalized)] = raw
		}
	}
	return idx
}

func (idx *pathIndex) resolve(normalized string) (string, bool) {
	if original, ok := idx.byPath[normalized]; ok {
		return original, true
	}
	// A repository path that is a tail of the entity: the maker's own directories
	// sit above the repo root, so prefer the longest (most specific) agreement.
	bestKey, bestOriginal := "", ""
	for repoPath, original := range idx.byPath {
		if !strings.HasSuffix(normalized, "/"+repoPath) {
			continue
		}
		if len(repoPath) > len(bestKey) || (len(repoPath) == len(bestKey) && repoPath < bestKey) {
			bestKey, bestOriginal = repoPath, original
		}
	}
	if bestKey != "" {
		return bestOriginal, true
	}
	if original, ok := idx.byBase[path.Base(normalized)]; ok {
		return original, true
	}
	return "", false
}

func statusRank(status string) int {
	switch status {
	case "head":
		return 2
	case "history":
		return 1
	}
	return 0
}

// ResolveFileHours maps per-file coding time onto the repository, one row per
// resolved path plus a zero-second row for every default-branch file no time
// resolved to, so the browser shows the whole repository rather than only where
// the time went. headFiles is the default branch (original path -> size in bytes);
// historyPaths is every path the captured commits touched. Entities the repository
// cannot explain keep only their tail. Rows resolving to the same path merge:
// several people (or several local checkouts) editing one file are one file.
func ResolveFileHours(entitySeconds map[string]float64, headFiles map[string]int64, historyPaths []string) []FileHours {
	headPaths := make([]string, 0, len(headFiles))
	for headPath := range headFiles {
		headPaths = append(headPaths, headPath)
	}
	head := newPathIndex(headPaths)
	history := newPathIndex(historyPaths)

	merged := map[string]*FileHours{}
	for entity, seconds := range entitySeconds {
		if seconds <= 0 {
			continue
		}
		normalized := normalize(entity)
		if normalized == "" || normalized == "." {
			continue
		}
		row := FileHours{Path: Tail(entity), Seconds: seconds, Status: "none"}
		if original, ok := head.resolve(normalized); ok {
			row = FileHours{Path: original, Seconds: seconds, Bytes: headFiles[original], Status: "head"}
		} else if original, ok := history.resolve(normalized); ok {
			row = FileHours{Path: original, Seconds: seconds, Status: "history"}
		}
		existing, seen := merged[row.Path]
		if !seen {
			merged[row.Path] = &row
			continue
		}
		existing.Seconds += seconds
		if statusRank(row.Status) > statusRank(existing.Status) {
			existing.Status = row.Status
			existing.Bytes = row.Bytes
		}
	}

	for headPath, size := range headFiles {
		if _, taken := merged[headPath]; taken {
			continue
		}
		merged[headPath] = &FileHours{Path: headPath, Bytes: size, Status: "head"}
	}

	out := make([]FileHours, 0, len(merged))
	for _, row := range merged {
		out = append(out, *row)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Seconds != out[b].Seconds {
			return out[a].Seconds > out[b].Seconds
		}
		return out[a].Path < out[b].Path
	})
	return out
}

func normalize(filePath string) string {
	return strings.ToLower(strings.TrimPrefix(path.Clean(strings.ReplaceAll(filePath, "\\", "/")), "./"))
}

// the last three segments of a local path, so a maker's home directory never
// reaches an organizer's screen
func Tail(entity string) string {
	segments := strings.Split(strings.ReplaceAll(entity, "\\", "/"), "/")
	var kept []string
	for _, segment := range segments {
		if segment != "" {
			kept = append(kept, segment)
		}
	}
	if len(kept) > 3 {
		kept = kept[len(kept)-3:]
	}
	return strings.Join(kept, "/")
}
