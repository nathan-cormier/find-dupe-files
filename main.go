// Command find-dupe-files finds duplicate files using fclones for content hashing
// and grouping, then optionally deletes the duplicates.
//
// Usage:
//
//	find-dupe-files [flags] <input> <directory>
//
// The input may be a regular file or a directory:
//
//   - File input: groups identical files across the input file and the directory
//     tree, locates the group containing the input file, and prints the matching
//     paths under the directory (one per line). The input file is omitted from the
//     output unless -include-self is given. Deletion isolates the input's group into
//     a one-group report, lists the input first, and runs `fclones remove --priority
//     bottom` to remove every other member while keeping the input.
//
//   - Directory input: compares the input tree against the target directory and
//     prints files under the target whose content also exists under the input.
//     Deletion runs `fclones remove` restricted to the target tree (--path) and
//     protecting the input tree (--keep-path), so the input copies are kept.
//
// In both modes, when duplicates are found it offers to delete them. The prompt
// appears only when stdin is a terminal; -yes deletes non-interactively. fclones
// only ever touches the duplicates being removed; unrelated groups are left alone.
//
// The content hash function is selected with -hash-fn (passed through to
// `fclones group --hash-fn`). The set of valid values depends on your installed
// fclones version — run `fclones group --help` to list them.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "find-dupe-files:", err)
		os.Exit(1)
	}
}

func run() error {
	hashFn := flag.String("hash-fn", "xxhash", "fclones content hash function (run `fclones group --help` for valid values)")
	fclonesBin := flag.String("fclones", "", "path to the fclones binary (default: found on PATH)")
	includeSelf := flag.Bool("include-self", false, "also print the input file itself among the matches")
	assumeYes := flag.Bool("yes", false, "delete duplicates without prompting (also enables deletion when stdin is not a terminal)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <input-file-or-dir> <directory>\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 2 {
		flag.Usage()
		return errors.New("expected exactly two arguments: <input-file> <directory>")
	}

	inputPath, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		return fmt.Errorf("resolve input file: %w", err)
	}
	dirPath, err := filepath.Abs(flag.Arg(1))
	if err != nil {
		return fmt.Errorf("resolve directory: %w", err)
	}

	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("input: %w", err)
	}
	dirInfo, err := os.Stat(dirPath)
	if err != nil {
		return fmt.Errorf("directory: %w", err)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("not a directory: %s", dirPath)
	}

	bin := *fclonesBin
	if bin == "" {
		bin, err = exec.LookPath("fclones")
		if err != nil {
			return fmt.Errorf("fclones not found in PATH (install from https://github.com/pkolaczk/fclones, or pass -fclones <path>): %w", err)
		}
	}

	switch {
	case inputInfo.IsDir():
		return runDirMode(bin, *hashFn, inputPath, dirPath, *includeSelf, *assumeYes)
	case inputInfo.Mode().IsRegular():
		return runFileMode(bin, *hashFn, inputPath, dirPath, inputInfo, *includeSelf, *assumeYes)
	default:
		return fmt.Errorf("input is neither a regular file nor a directory: %s", inputPath)
	}
}

// runGroup runs `fclones group` over the given roots and returns the parsed report.
// fclones progress and errors stream to our stderr.
func runGroup(bin, hashFn string, roots ...string) (header []string, groups []group, err error) {
	args := append([]string{"group", "--hash-fn", hashFn, "--format", "default"}, roots...)
	cmd := exec.Command(bin, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr // let fclones progress and errors stream through
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("fclones group: %w", err)
	}
	header, groups = parseDefaultReport(stdout.Bytes())
	return header, groups, nil
}

// runFileMode handles a single regular-file input: find the duplicate group that
// contains it, print the other members, and offer to delete them (keeping the input).
func runFileMode(bin, hashFn, inputPath, dirPath string, inputInfo os.FileInfo, includeSelf, assumeYes bool) error {
	// Decide which roots to scan. If the input file lives inside the directory
	// tree, scanning the directory alone already covers it; passing it again as a
	// separate root is redundant. Otherwise we add it so fclones compares it
	// against the tree.
	roots := []string{dirPath}
	if !withinDir(inputPath, dirPath) {
		roots = append([]string{inputPath}, roots...)
	}

	header, groups, err := runGroup(bin, hashFn, roots...)
	if err != nil {
		return err
	}
	target := findGroupWithFile(groups, inputInfo)
	if target == nil {
		fmt.Fprintf(os.Stderr, "no duplicate of %s found under %s\n", inputPath, dirPath)
		return nil
	}

	// Partition the group into the input file itself and its duplicates.
	var selfMembers, dupes []member
	for _, m := range target.members {
		if sameFile(m.path, inputInfo) {
			selfMembers = append(selfMembers, m)
		} else {
			dupes = append(dupes, m)
		}
	}

	printed := dupes
	if includeSelf {
		printed = target.members
	}
	for _, m := range printed {
		fmt.Println(m.path)
	}

	if len(dupes) == 0 {
		return nil
	}

	if !confirmDelete(len(dupes), inputPath, assumeYes) {
		return nil
	}

	// Build a report containing ONLY the input's group, with the input listed
	// first, then run `fclones remove --priority bottom`. Verified behavior:
	// priority "bottom" keeps the file listed first and removes the rest, and
	// rf-over defaults to 1 (keep one replica). The original report header is
	// preserved verbatim so fclones' timestamp safety check still passes.
	filtered := buildSingleGroupReport(header, target.header, selfMembers, dupes)
	rm := exec.Command(bin, "remove", "--priority", "bottom")
	rm.Stdin = strings.NewReader(filtered)
	rm.Stdout = os.Stderr // fclones remove logs to stderr; keep our stdout clean
	rm.Stderr = os.Stderr
	if err := rm.Run(); err != nil {
		return fmt.Errorf("fclones remove: %w", err)
	}

	// Sanity check: the input file must survive. If it somehow doesn't, fail loudly.
	if _, err := os.Stat(inputPath); err != nil {
		return fmt.Errorf("CRITICAL: input file no longer exists after deletion (%s): %w", inputPath, err)
	}
	return nil
}

// runDirMode handles a directory input: it compares the input tree against the
// target tree and finds files under the target whose content also exists under the
// input. Those target-side files are printed and offered for deletion, keeping the
// input tree's copies. Deletion is delegated to fclones and restricted to the
// target tree, so input files (including input-internal duplicates) are never
// removed, and unrelated duplicate groups in the target are left alone.
func runDirMode(bin, hashFn, srcDir, dstDir string, includeSelf, assumeYes bool) error {
	// Canonicalize so paths compare cleanly by prefix against fclones' canonical
	// report paths (e.g. /var -> /private/var on macOS).
	csrc := canonicalDir(srcDir)
	cdst := canonicalDir(dstDir)
	if csrc == cdst {
		return fmt.Errorf("input directory and target directory are the same: %s", csrc)
	}

	header, groups, err := runGroup(bin, hashFn, srcDir, dstDir)
	if err != nil {
		return err
	}

	// Keep only "cross" groups: at least one file under the input tree AND at least
	// one duplicate under the target tree. A file under both (overlapping trees) is
	// treated as belonging to the input tree, so it is protected from deletion.
	var crossGroups []group
	dstCount := 0
	for _, g := range groups {
		hasSrc, dstN := false, 0
		for _, m := range g.members {
			switch {
			case underDir(m.path, csrc):
				hasSrc = true
			case underDir(m.path, cdst):
				dstN++
			}
		}
		if hasSrc && dstN > 0 {
			crossGroups = append(crossGroups, g)
			dstCount += dstN
		}
	}

	if dstCount == 0 {
		fmt.Fprintf(os.Stderr, "no files under %s duplicate content under %s\n", dstDir, srcDir)
		return nil
	}

	for _, g := range crossGroups {
		for _, m := range g.members {
			if includeSelf || (underDir(m.path, cdst) && !underDir(m.path, csrc)) {
				fmt.Println(m.path)
			}
		}
	}

	// fclones remove selects files to keep/drop by glob pattern; if a directory
	// path contains glob metacharacters we cannot build a safe pattern, so we refuse
	// to delete (the listing above still succeeded).
	if hasGlobMeta(csrc) || hasGlobMeta(cdst) {
		fmt.Fprintf(os.Stderr, "not deleting: a directory path contains glob metacharacters (%q or %q); remove duplicates manually\n", csrc, cdst)
		return nil
	}

	if !confirmDelete(dstCount, srcDir, assumeYes) {
		return nil
	}

	// Restrict removal to the target tree and explicitly protect the input tree.
	// Verified: this removes only target-side duplicates and keeps every input file
	// (including input-internal duplicates). The filtered report contains only cross
	// groups, so unrelated duplicate groups are never touched.
	filtered := buildReport(header, crossGroups)
	rm := exec.Command(bin, "remove", "--path", cdst+"/**", "--keep-path", csrc+"/**")
	rm.Stdin = strings.NewReader(filtered)
	rm.Stdout = os.Stderr // fclones remove logs to stderr; keep our stdout clean
	rm.Stderr = os.Stderr
	if err := rm.Run(); err != nil {
		return fmt.Errorf("fclones remove: %w", err)
	}

	// Sanity check: the input directory must survive.
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("CRITICAL: input directory no longer exists after deletion (%s): %w", srcDir, err)
	}
	return nil
}

// confirmDelete returns whether the duplicates should be deleted. With assumeYes it
// is always true. Otherwise it prompts (default yes) when stdin is a terminal, and
// declines without prompting when stdin is not a terminal (safer for pipelines).
func confirmDelete(n int, keep string, assumeYes bool) bool {
	if assumeYes {
		return true
	}
	if !isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "stdin is not a terminal; not deleting (use -yes to delete non-interactively)")
		return false
	}
	fmt.Fprintf(os.Stderr, "Delete %d duplicate file(s), keeping %s? (Y/n) ", n, keep)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		fmt.Fprintln(os.Stderr, "Aborted; no files deleted.")
		return false
	}
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// withinDir reports whether path is dir itself or lies inside the dir tree.
func withinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonicalDir resolves symlinks so a directory can be compared by prefix against
// the canonical paths fclones reports (e.g. /var -> /private/var on macOS). Falls
// back to the cleaned absolute path if resolution fails.
func canonicalDir(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// underDir reports whether path equals dir or lies within it. Both must be canonical.
func underDir(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// hasGlobMeta reports whether s contains characters that fclones would interpret as
// glob metacharacters, which would make a path-based --path/--keep-path unsafe.
func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, `*?[]{}\`)
}

// buildReport reconstructs a default-format report from the original header and the
// given groups (members preserved verbatim, including indentation).
func buildReport(header []string, groups []group) string {
	var b strings.Builder
	for _, h := range header {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	for _, g := range groups {
		b.WriteString(g.header)
		b.WriteByte('\n')
		for _, m := range g.members {
			b.WriteString(m.raw)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// member is one file line within a group: the raw report line (indentation
// preserved) and the trimmed path.
type member struct {
	raw  string
	path string
}

// group is one duplicate group from the default-format report: the raw group
// header line (hash/size/count) and its member files.
type group struct {
	header  string
	members []member
}

// parseDefaultReport splits fclones' default-format report into its comment header
// lines and duplicate groups. In that format, comment lines start with '#', a group
// header line is non-indented (hash, size, count), and member paths are indented.
func parseDefaultReport(data []byte) (header []string, groups []group) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	curIdx := -1
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "#"):
			header = append(header, line)
		case strings.TrimSpace(line) == "":
			// blank line: ignore
		case strings.HasPrefix(line, " "):
			if curIdx >= 0 {
				groups[curIdx].members = append(groups[curIdx].members, member{raw: line, path: strings.TrimSpace(line)})
			}
		default:
			groups = append(groups, group{header: line})
			curIdx = len(groups) - 1
		}
	}
	return header, groups
}

// findGroupWithFile returns the first group containing a path that refers to the
// same on-disk file as want (compared by device+inode), or nil if none do.
func findGroupWithFile(groups []group, want os.FileInfo) *group {
	for i := range groups {
		for _, m := range groups[i].members {
			if sameFile(m.path, want) {
				return &groups[i]
			}
		}
	}
	return nil
}

// buildSingleGroupReport reconstructs a default-format report with the original
// header and exactly one group, listing the input's own member lines first so that
// `fclones remove --priority bottom` keeps the input and removes the duplicates.
func buildSingleGroupReport(header []string, groupHeader string, self, dupes []member) string {
	var b strings.Builder
	for _, h := range header {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	b.WriteString(groupHeader)
	b.WriteByte('\n')
	for _, m := range self {
		b.WriteString(m.raw)
		b.WriteByte('\n')
	}
	for _, m := range dupes {
		b.WriteString(m.raw)
		b.WriteByte('\n')
	}
	return b.String()
}

func sameFile(path string, want os.FileInfo) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(info, want)
}
