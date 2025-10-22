package tmpdir

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/containers/buildah/internal/httpclient"
	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/chrootarchive"
	"go.podman.io/storage/pkg/ioutils"
)

type URLOptions = httpclient.URLOptions

// ForURL checks if the passed-in string looks like a URL or "-".  If it is,
// ForURL creates a temporary directory, arranges for the contents of one of
// its subdirectories to be the contents of that URL, and returns the temporary
// directory's path, along with the relative name of a subdirectory (which may
// be empty or ".").  When the URL string specifies a build context, the
// subdirectory should be used as the build context.
// Removal of the temporary directory is the responsibility of the caller.
// If the string doesn't look like a URL or "-", ForURL returns empty strings
// and a nil error code.
func ForURL(dir, prefix, url string, options *URLOptions) (name, subdir string, err error) {
	if options == nil {
		options = &URLOptions{}
	}
	if !strings.HasPrefix(url, "http://") &&
		!strings.HasPrefix(url, "https://") &&
		!strings.HasPrefix(url, "git://") &&
		!strings.HasPrefix(url, "github.com/") &&
		url != "-" {
		return "", "", nil
	}
	name, err = os.MkdirTemp(dir, prefix)
	if err != nil {
		return "", "", fmt.Errorf("creating temporary directory for %q: %w", url, err)
	}
	downloadDir := filepath.Join(name, "download")
	if err = os.MkdirAll(downloadDir, 0o700); err != nil {
		return "", "", fmt.Errorf("creating directory %q for %q: %w", downloadDir, url, err)
	}
	urlParsed, err := urlpkg.Parse(url)
	if err != nil {
		return "", "", fmt.Errorf("parsing url %q: %w", url, err)
	}
	if strings.HasPrefix(url, "git://") || strings.HasSuffix(urlParsed.Path, ".git") {
		combinedOutput, gitSubDir, err := cloneToDirectory(url, downloadDir)
		if err != nil {
			if err2 := os.RemoveAll(name); err2 != nil {
				logrus.Debugf("error removing temporary directory %q: %v", name, err2)
			}
			return "", "", fmt.Errorf("cloning %q to %q:\n%s: %w", url, name, string(combinedOutput), err)
		}
		logrus.Debugf("Build context is at %q", filepath.Join(downloadDir, gitSubDir))
		return name, filepath.Join(filepath.Base(downloadDir), gitSubDir), nil
	}
	if strings.HasPrefix(url, "github.com/") {
		ghurl := url
		url = fmt.Sprintf("https://%s/archive/master.tar.gz", ghurl)
		logrus.Debugf("resolving url %q to %q", ghurl, url)
		subdir = path.Base(ghurl) + "-master"
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		err = downloadToDirectory(*options, url, downloadDir)
		if err != nil {
			if err2 := os.RemoveAll(name); err2 != nil {
				logrus.Debugf("error removing temporary directory %q: %v", name, err2)
			}
			return "", "", err
		}
		logrus.Debugf("Build context is at %q", filepath.Join(downloadDir, subdir))
		return name, filepath.Join(filepath.Base(downloadDir), subdir), nil
	}
	if url == "-" {
		err = stdinToDirectory(downloadDir)
		if err != nil {
			if err2 := os.RemoveAll(name); err2 != nil {
				logrus.Debugf("error removing temporary directory %q: %v", name, err2)
			}
			return "", "", err
		}
		logrus.Debugf("Build context is at %q", filepath.Join(downloadDir, subdir))
		return name, filepath.Join(filepath.Base(downloadDir), subdir), nil
	}
	logrus.Debugf("don't know how to retrieve %q", url)
	if err2 := os.RemoveAll(name); err2 != nil {
		logrus.Debugf("error removing temporary directory %q: %v", name, err2)
	}
	return "", "", errors.New("unreachable code reached")
}

// parseGitBuildContext parses git build context to `repo`, `sub-dir`
// `branch/commit`, accepts GitBuildContext in the format of
// `repourl.git[#[branch-or-commit]:subdir]`.
func parseGitBuildContext(url string) (string, string, string) {
	gitSubdir := ""
	gitBranch := ""
	gitBranchPart := strings.Split(url, "#")
	if len(gitBranchPart) > 1 {
		// check if string contains path to a subdir
		gitSubDirPart := strings.Split(gitBranchPart[1], ":")
		if len(gitSubDirPart) > 1 {
			gitSubdir = gitSubDirPart[1]
		}
		gitBranch = gitSubDirPart[0]
	}
	return gitBranchPart[0], gitSubdir, gitBranch
}

func cloneToDirectory(url, dir string) ([]byte, string, error) {
	var cmd *exec.Cmd
	gitRepo, gitSubdir, gitRef := parseGitBuildContext(url)
	// init repo
	cmd = exec.Command("git", "init", dir)
	combinedOutput, err := cmd.CombinedOutput()
	if err != nil {
		// Return err.Error() instead of err as we want buildah to override error code with more predictable
		// value.
		return combinedOutput, gitSubdir, fmt.Errorf("failed while performing `git init`: %s", err.Error())
	}
	// add origin
	cmd = exec.Command("git", "remote", "add", "origin", gitRepo)
	cmd.Dir = dir
	combinedOutput, err = cmd.CombinedOutput()
	if err != nil {
		// Return err.Error() instead of err as we want buildah to override error code with more predictable
		// value.
		return combinedOutput, gitSubdir, fmt.Errorf("failed while performing `git remote add`: %s", err.Error())
	}

	logrus.Debugf("fetching repo %q and branch (or commit ID) %q to %q", gitRepo, gitRef, dir)
	args := []string{"fetch", "-u", "--depth=1", "origin", "--", gitRef}
	cmd = exec.Command("git", args...)
	cmd.Dir = dir
	combinedOutput, err = cmd.CombinedOutput()
	if err != nil {
		// Return err.Error() instead of err as we want buildah to override error code with more predictable
		// value.
		return combinedOutput, gitSubdir, fmt.Errorf("failed while performing `git fetch`: %s", err.Error())
	}

	cmd = exec.Command("git", "checkout", "FETCH_HEAD")
	cmd.Dir = dir
	combinedOutput, err = cmd.CombinedOutput()
	if err != nil {
		// Return err.Error() instead of err as we want buildah to override error code with more predictable
		// value.
		return combinedOutput, gitSubdir, fmt.Errorf("failed while performing `git checkout`: %s", err.Error())
	}
	return combinedOutput, gitSubdir, nil
}

func downloadToDirectory(options URLOptions, url, dir string) error {
	logrus.Debugf("extracting %q to %q", url, dir)

	httpClient, err := httpclient.ForURLOptions(options)
	if err != nil {
		return err
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("invalid response status %d", resp.StatusCode)
	}
	if resp.ContentLength == 0 {
		return fmt.Errorf("no contents in %q", url)
	}
	if err := chrootarchive.Untar(resp.Body, dir, nil); err != nil {
		resp1, err := http.Get(url)
		if err != nil {
			return err
		}
		defer resp1.Body.Close()
		body, err := io.ReadAll(resp1.Body)
		if err != nil {
			return err
		}
		dockerfile := filepath.Join(dir, "Dockerfile")
		// Assume this is a Dockerfile
		if err := ioutils.AtomicWriteFile(dockerfile, body, 0o600); err != nil {
			return fmt.Errorf("failed to write %q to %q: %w", url, dockerfile, err)
		}
	}
	return nil
}

func stdinToDirectory(dir string) error {
	logrus.Debugf("extracting stdin to %q", dir)
	r := bufio.NewReader(os.Stdin)
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("failed to read from stdin: %w", err)
	}
	reader := bytes.NewReader(b)
	if err := chrootarchive.Untar(reader, dir, nil); err != nil {
		dockerfile := filepath.Join(dir, "Dockerfile")
		// Assume this is a Dockerfile
		if err := ioutils.AtomicWriteFile(dockerfile, b, 0o600); err != nil {
			return fmt.Errorf("failed to write bytes to %q: %w", dockerfile, err)
		}
	}
	return nil
}
