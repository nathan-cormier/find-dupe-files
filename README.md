# find-dupe-files

A small wrapper around [`fclones`](https://github.com/pkolaczk/fclones) that finds duplicate files and offers to delete them. The first argument (the **input**) may be a **file** or a **directory**:

- **File input** — prints every file in the target directory tree whose content is identical to the input file.
- **Directory input** — compares the input directory against the target directory and prints every file in the target tree whose content also exists in the input tree.

It uses fclones to scan and content-hash the trees. In file mode the input file itself is omitted from the output unless `-include-self` is given.

After listing, if duplicates were found it offers to **delete** them — keeping the input file (file mode) or the input tree's copies (directory mode) — performing the deletion with fclones itself. The prompt only appears when stdin is a terminal; use `-yes` to delete non-interactively.

## Requirements

- Go 1.21+
- [`fclones`](https://github.com/pkolaczk/fclones) on your `PATH` (or pass `-fclones <path>`). Install via `brew install fclones` / `cargo install fclones`.

Verified against fclones **0.35.0**.

## Build

```sh
go build ./cmd/find-dupe-files
```

## Usage

```
find-dupe-files [flags] <input-file-or-dir> <directory>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-hash-fn` | `xxhash` | fclones content hash function. Run `fclones group --help` for the valid values on your version. |
| `-fclones` | (found on `PATH`) | Path to the fclones binary. |
| `-include-self` | `false` | Also print the input file itself among the matches. |
| `-yes` | `false` | Delete duplicates without prompting. Also enables deletion when stdin is not a terminal. |

### Output and exit codes

- Matching paths are printed to **stdout**, one per line — clean for piping (`xargs`, etc.).
- fclones' progress/log output (including deletion logs) goes to **stderr**.
- If no duplicate is found, a notice is printed to stderr and the program exits `0`.
- Errors (missing file, fclones failure, bad hash function) exit `1`.

### Deleting duplicates

When duplicates are found, the tool offers to delete them while keeping the input file:

- With `-yes`, it deletes immediately, no prompt.
- Otherwise, if **stdin is a terminal**, it prompts `Delete N duplicate file(s), keeping <input>? (Y/n)`. The default (just pressing Enter) is **yes**.
- If **stdin is not a terminal** and `-yes` was not given, it prints a notice and deletes nothing — so `... | xargs rm` style pipelines never trigger an accidental delete.

Deletion is delegated to fclones (see below). In file mode only the input file's duplicates are removed; in directory mode only the target tree's files that duplicate input-tree content are removed. Other duplicate groups in the tree are never touched, and the kept input (file or directory) is verified to still exist afterward.

## Examples

Find every copy of `photo.jpg` under a backup tree:

```sh
$ find-dupe-files photo.jpg /Volumes/Backup
/Volumes/Backup/2023/photo.jpg
/Volumes/Backup/imports/photo (1).jpg
```

Use a file already inside the tree as the query — it is excluded from its own results:

```sh
$ find-dupe-files /Volumes/Backup/2023/photo.jpg /Volumes/Backup
/Volumes/Backup/imports/photo (1).jpg
```

Find duplicates and be prompted to delete them (keeping `photo.jpg`):

```sh
$ find-dupe-files photo.jpg /Volumes/Backup
/Volumes/Backup/2023/photo.jpg
/Volumes/Backup/imports/photo (1).jpg
Delete 2 duplicate file(s), keeping /path/to/photo.jpg? (Y/n) y
```

Delete without prompting (e.g. in a script):

```sh
$ find-dupe-files -yes photo.jpg /Volumes/Backup
```

Compare two directories — find files in `/Volumes/Backup` that already exist in `/Volumes/Camera`, and delete those backup-side copies (keeping the `/Volumes/Camera` originals):

```sh
$ find-dupe-files /Volumes/Camera /Volumes/Backup
/Volumes/Backup/2023/IMG_0001.JPG
/Volumes/Backup/dupes/IMG_0001.JPG
Delete 2 duplicate file(s), keeping /Volumes/Camera? (Y/n) y
```

Pick a different hash function (e.g. cryptographic):

```sh
$ find-dupe-files -hash-fn blake3 photo.jpg /Volumes/Backup
```

## How matching works (file mode)

- The directory is passed to fclones as the scan root. The input file is added as a
  second root **only when it lives outside** the tree, so it is never scanned twice.
- fclones reports duplicate groups in its default format. The group containing the
  input file is identified by **device + inode identity** (`os.SameFile`), not by
  string-comparing paths — so symlinks and path-formatting differences don't break
  the match.
- The matched group's other members are printed; the input file is filtered out by
  the same inode check unless `-include-self` is set.

## How matching works (directory mode)

- Both the input directory and the target directory are passed to fclones as scan
  roots, and the report is grouped as usual.
- Only **cross groups** are kept: groups that contain at least one file under the
  input tree *and* at least one duplicate under the target tree. Groups that live
  entirely within one tree (e.g. a duplicate pair that exists only in the target)
  are ignored — this tool reports content shared *between* the two trees, not
  internal duplicates.
- For each cross group, the target-tree members are printed (or, with
  `-include-self`, all members). Membership is decided by canonical-path prefix:
  paths are resolved with `EvalSymlinks` so `/var` vs `/private/var` on macOS
  compares correctly.

### How deletion works

Deletion reuses the single `fclones group` run rather than calling `rm`:

1. The original report is filtered down to **only the input file's group**, with the
   input file listed first. The report header is preserved verbatim so fclones'
   timestamp safety check (which refuses to act on files modified after the scan)
   still passes.
2. That one-group report is piped to `fclones remove --priority bottom`, which keeps
   the file listed first (the input) and removes the rest (its duplicates).

Because the report contains only the input's group, no other duplicates in the tree
can be affected. Identifying the input by inode (not by path string) is what makes
this safe on macOS, where fclones canonicalizes `/var/...` to `/private/var/...` and
a path-pattern `--keep-path` could otherwise fail to match.

In **directory mode**, the report is filtered down to the cross groups, then piped to:

```
fclones remove --path '<target-dir>/**' --keep-path '<input-dir>/**'
```

`--path` restricts the removable set to the target tree, and `--keep-path` explicitly
protects the input tree (defense in depth). The result keeps every input-tree file —
including the input tree's own internal duplicates — and removes only the target-tree
files that duplicate input-tree content.

## Notes and caveats

- **Hash function / XXH128.** fclones 0.35.0 exposes a single `xxhash` option (no
  separately selectable 128-bit XXH128 variant). `xxhash` is fast and more than
  adequate for content identity. If you want a wider, cryptographic digest, use
  `-hash-fn blake3`. The accepted tokens depend on your fclones version — confirm
  with `fclones group --help`.
- **Empty files.** fclones applies its own selection criteria and, by default, does
  not group zero-byte files, so an empty input file will report no matches.
- **Deletion keeps the input.** File mode keeps the input file; directory mode keeps
  the whole input tree. The prompt is skipped (no deletion) when stdin is not a
  terminal unless `-yes` is given.
- **Directory-mode glob metacharacters.** Directory-mode deletion builds fclones
  `--path`/`--keep-path` glob patterns from the directory paths. If either canonical
  path contains glob metacharacters (`* ? [ ] { } \`), the tool prints the duplicates
  but refuses to delete (to avoid an unsafe pattern); remove those manually.
