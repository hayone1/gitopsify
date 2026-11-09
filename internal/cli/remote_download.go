package cli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newRunDownload(o *options, rf *remoteFlags) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "download <id>",
		Short: "Download the files a run wrote",
		Long: `Downloads the files a migration or export wrote on the server into a new
directory (-o; default <name>-<id>). Secret values the server keeps (git-ignored
files) aren't included.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			u := o.ui()
			c, err := rf.client()
			if err != nil {
				return err
			}
			body, err := c.Archive(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer body.Close()
			n, top, err := extractRun(body, dir)
			if err != nil {
				return err
			}
			u.Success("downloaded %d files into %s", n, firstOf(dir, top))
			return nil
		},
	}
	cmd.Flags().StringVarP(&dir, "output", "o", "", "directory to write into; must be new or empty (default <name>-<id>)")
	return cmd
}

// extractRun writes a run archive (every entry under one top directory)
// into dir, or the top directory's name. Only regular files and
// directories are written, inside dir, through an os.Root.
func extractRun(r io.Reader, dir string) (int, string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, "", fmt.Errorf("reading the archive: %w", err)
	}
	tr := tar.NewReader(gz)
	var root *os.Root
	defer func() {
		if root != nil {
			root.Close()
		}
	}()
	files, top := 0, ""
	var total int64
	const maxFile, maxTotal, maxEntries = 1 << 30, 8 << 30, 200000
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, top, fmt.Errorf("reading the archive: %w", err)
		}
		if entries++; entries > maxEntries {
			return files, top, errors.New("the archive has too many entries")
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		first, rest, _ := strings.Cut(name, "/")
		if top == "" {
			top = first
			if dir == "" {
				dir = top
			}
			if strings.Contains(top, "..") || top == "." || top == "" || !filepath.IsLocal(top) {
				return 0, "", fmt.Errorf("unexpected archive entry %q", h.Name)
			}
			if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
				return 0, "", fmt.Errorf("%s isn't empty: pick a new directory with -o", dir)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return 0, "", err
			}
			if root, err = os.OpenRoot(dir); err != nil {
				return 0, "", err
			}
		}
		if first != top || rest == "" {
			continue
		}
		if path.IsAbs(rest) || rest == ".." || strings.HasPrefix(rest, "../") {
			return files, top, fmt.Errorf("unexpected archive entry %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rest, 0o755); err != nil {
				return files, top, err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(path.Dir(rest), 0o755); err != nil {
				return files, top, err
			}
			mode := os.FileMode(0o644)
			if h.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			f, err := root.OpenFile(rest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return files, top, err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxFile+1))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, top, err
			}
			if total += n; n > maxFile || total > maxTotal {
				return files, top, fmt.Errorf("%s: the archive is larger than gitopsify downloads (1 GiB a file, 8 GiB in all)", rest)
			}
			files++
		}
	}
	if top == "" {
		return 0, "", errors.New("the run wrote no files")
	}
	return files, top, nil
}
