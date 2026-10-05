package githost

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var trailingGit = regexp.MustCompile(`(?i)\.git$`)

func NormalizeRepoUrl(repoUrl string) string {
	return strings.TrimSuffix(trailingGit.ReplaceAllString(repoUrl, ""), "/")
}

// ParseCoAuthors parses the Co-authored-by trailer block (one "Name <email>"
// per line). Tolerates a bare name or bare email; deduped and capped at 20.
func ParseCoAuthors(raw string) []CoAuthor {
	var out []CoAuthor
	seen := map[string]bool{}
	nameEmail := regexp.MustCompile(`^(.*?)\s*<([^>]+)>\s*$`)
	for _, line := range strings.Split(raw, "\n") {
		v := strings.TrimSpace(line)
		if v == "" {
			continue
		}
		var name, email string
		if m := nameEmail.FindStringSubmatch(v); m != nil {
			name = sliceRunes(strings.TrimSpace(m[1]), 120)
			email = sliceRunes(strings.ToLower(strings.TrimSpace(m[2])), 254)
		} else {
			name = sliceRunes(v, 120)
		}
		key := email
		if key == "" {
			key = strings.ToLower(name)
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, CoAuthor{Name: nilIfEmpty(name), Email: nilIfEmpty(email)})
		if len(out) >= 20 {
			break
		}
	}
	return out
}

// the fields every backend fills the same way, so a commit reads identically
// whichever one produced it
func newCommit(hash, subject, authorName, authorEmail, coAuthorLines, webUrl string, committedAt time.Time) Commit {
	if subject == "" {
		subject = "(no message)"
	}
	return Commit{
		Hash:        hash,
		ShortHash:   hash[:7],
		Message:     sliceRunes(subject, 200),
		CommittedAt: committedAt,
		AuthorName:  sliceRunes(strings.TrimSpace(authorName), 120),
		AuthorEmail: sliceRunes(strings.ToLower(strings.TrimSpace(authorEmail)), 254),
		CoAuthors:   ParseCoAuthors(coAuthorLines),
		Url:         webUrl + "/commit/" + hash,
	}
}

// ParseLog parses the output of logArgs: one "\x01<header>\x02" record per commit,
// followed by the NUL separated paths the commit touched.
func ParseLog(stdout, repoUrl string) []Commit {
	base := NormalizeRepoUrl(repoUrl)
	var commits []Commit
	for _, block := range strings.Split(stdout, "\x01") {
		header, rest, hasPaths := strings.Cut(block, "\x02")
		if !hasPaths {
			continue
		}
		fields := strings.SplitN(header, "\x1f", 6)
		if len(fields) != 6 {
			continue
		}
		hash := fields[0]
		if len(hash) != 40 || !isHex(hash) {
			continue
		}
		committedAt, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			continue
		}
		commit := newCommit(hash, fields[4], fields[2], fields[3], fields[5], base, committedAt)
		for _, name := range strings.Split(rest, "\x00") {
			if name = strings.Trim(name, "\n"); name != "" {
				commit.Paths = append(commit.Paths, name)
			}
		}
		commits = append(commits, commit)
	}
	return commits
}

// git's %s: the first paragraph of the message folded onto one line
func subjectOf(message string) string {
	var parts []string
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " ")
}

// position of the ":" that makes a line a trailer, as git's find_separator reads it
func trailerSeparator(line string) int {
	spaced := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ':':
			return i
		case !spaced && (c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')):
		case i > 0 && (c == ' ' || c == '\t'):
			spaced = true
		default:
			return -1
		}
	}
	return -1
}

// coAuthorTrailers reads a full commit message the way
// %(trailers:key=Co-authored-by,valueonly) does: only the trailer block counts,
// which is the last paragraph when git accepts it as one, never the title.
func coAuthorTrailers(message string) string {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	blank := func(line string) bool { return strings.TrimSpace(line) == "" }
	indented := func(line string) bool { return line[0] == ' ' || line[0] == '\t' }

	titleEnd := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if blank(line) {
			titleEnd = i
			break
		}
	}

	start := -1
	onlySpaces, generated := true, false
	trailers, others, continuations := 0, 0, 0
	for i := len(lines) - 1; i >= titleEnd && start < 0; i-- {
		line := lines[i]
		if strings.HasPrefix(line, "#") {
			others += continuations
			continuations = 0
			continue
		}
		if blank(line) {
			if onlySpaces {
				continue
			}
			others += continuations
			if (generated && trailers*3 >= others) || (trailers > 0 && others == 0) {
				start = i + 1
			}
			break
		}
		onlySpaces = false
		switch {
		case strings.HasPrefix(line, "Signed-off-by: ") || strings.HasPrefix(line, "(cherry picked from commit "):
			trailers++
			continuations = 0
			generated = true
		case !indented(line) && trailerSeparator(line) >= 1:
			trailers++
			continuations = 0
		case indented(line):
			continuations++
		default:
			others += continuations + 1
			continuations = 0
		}
	}
	if start < 0 {
		return ""
	}

	var values []string
	open := false
	for _, line := range lines[start:] {
		if blank(line) {
			open = false
			continue
		}
		if indented(line) {
			if open {
				values = append(values, line)
			}
			continue
		}
		separator := trailerSeparator(line)
		open = separator >= 1 && strings.EqualFold(strings.TrimSpace(line[:separator]), "co-authored-by")
		if open {
			values = append(values, strings.TrimSpace(line[separator+1:]))
		}
	}
	return strings.Join(values, "\n")
}

// every path the commits touched, in first-seen order
func historyPaths(groups ...[]Commit) []string {
	seen := map[string]bool{}
	var out []string
	for _, commits := range groups {
		for _, commit := range commits {
			for _, changed := range commit.Paths {
				if !seen[changed] {
					seen[changed] = true
					out = append(out, changed)
				}
			}
		}
	}
	return out
}

// ParseTree parses `git ls-tree -r --long -z HEAD`: NUL-separated
// "<mode> <type> <blob> <size>\t<path>" records. Non-blob entries (submodules) are
// skipped. -z keeps paths verbatim, so spaces and non-ASCII names survive. A blob
// the clone never fetched has no readable size and comes back as UnknownBytes.
func ParseTree(stdout string) []File {
	var out []File
	for _, record := range strings.Split(stdout, "\x00") {
		meta, path, hasPath := strings.Cut(record, "\t")
		if !hasPath || path == "" {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		bytes, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			bytes = UnknownBytes
		}
		out = append(out, File{Path: path, Blob: fields[2], Bytes: bytes})
	}
	return out
}

// FindReadme picks the root README out of a listing: any extension or none, any
// casing. Returns "" when the listing has none at the repository root.
func FindReadme(names []string) string {
	for _, name := range names {
		if strings.Contains(name, "/") {
			continue // nested: the check asks for a README at the top level
		}
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "readme" || strings.HasPrefix(lower, "readme.") {
			return name
		}
	}
	return ""
}

// previewOf cuts a file's bytes down to what the source viewer carries
func previewOf(content string, truncated bool) FileContent {
	if len(content) > MaxPreviewBytes {
		content = content[:MaxPreviewBytes]
		truncated = true
	}
	probe := content
	if len(probe) > 8000 { // git's own text-vs-binary window
		probe = probe[:8000]
	}
	if strings.IndexByte(probe, 0) >= 0 {
		return FileContent{OK: true, Binary: true}
	}
	return FileContent{OK: true, Content: content, Truncated: truncated}
}

func errorCode(note string) string {
	code, _, _ := strings.Cut(note, ":")
	return code
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sliceRunes(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}
