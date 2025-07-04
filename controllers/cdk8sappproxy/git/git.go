package git

import (
	"bytes"
	"fmt"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitOperator defines the interface for git operations.
type GitOperator interface {
	Clone(repoUrl string, writer *bytes.Buffer) (directory string, err error)
	Poll(repo string, branch string, directory string, writer *bytes.Buffer) (changes bool, err error)
	Hash(repo string, branch string) (hash string, err error)
	CleanUp(directory string, maxAge time.Duration) (err error)
}

// GitImplementer implements the GitOperator interface.
type GitImplementer struct{}

// Clone clones the given repository to a local directory.
func (g *GitImplementer) Clone(repoUrl string, writer *bytes.Buffer) (directory string, err error) {
	tempDirPattern := "cdk8s-git-clone-"

	directory, err = os.MkdirTemp("", tempDirPattern)
	if err != nil {
		return directory, fmt.Errorf("failed to create temporary directory: %w", err)
	}

	// Check if repo and directory are empty.
	if empty(repoUrl, directory) {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.EmptyGitRepositoryReason)

		return directory, fmt.Errorf("%s", addonsv1alpha1.EmptyGitRepositoryReason)
	}

	_, err = git.PlainClone(directory, false, &git.CloneOptions{
		URL: repoUrl,
	})
	if err != nil {
		fmt.Fprintf(writer, addonsv1alpha1.GitCloneFailedCondition)

		return directory, err
	}

	return directory, err
}

// Poll polls for changes for the given remote git repository. Returns true, if current local commit hash and remote hash are not equal.
func (g *GitImplementer) Poll(repo string, branch string, directory string, writer *bytes.Buffer) (changes bool, err error) {
	// Defaults to false. We only change to true if there is a difference between the hashes.
	changes = false

	// Check if repo and directory are empty.
	if empty(repo, directory) {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.EmptyGitRepositoryReason)

		return changes, fmt.Errorf("%s", addonsv1alpha1.EmptyGitRepositoryReason)
	}

	// Get hash from local repo.
	localHash, err := g.Hash(directory, branch)
	if err != nil {
		fmt.Fprintf(writer, "localGitHash error")

		return changes, err
	}

	// Get Hash from remote repo
	remoteHash, err := g.Hash(repo, branch)
	if err != nil {
		fmt.Fprintf(writer, "remoteGitHash error")

		return changes, err
	}

	if localHash != remoteHash {
		changes = true
	}

	fmt.Fprintf(writer, "%s", addonsv1alpha1.GitHashSuccessReason)

	return changes, err
}

// Hash retrieves the hash of the given repository.
func (g *GitImplementer) Hash(repo string, branch string) (hash string, err error) {
	switch {
	case isUrl(repo):
		remoterepo := git.NewRemote(nil, &config.RemoteConfig{
			URLs: []string{repo},
			Name: "origin",
		})

		refs, err := remoterepo.List(&git.ListOptions{})
		if err != nil {
			//return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
			return hash, fmt.Errorf("failed to list remote refs: %w", err)
		}

		refName := plumbing.NewBranchReferenceName(branch)
		for _, ref := range refs {
			if ref.Name() == refName {
				return ref.Hash().String(), nil
			}
		}

		//return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
		return hash, fmt.Errorf("failed to find remote ref for branch %s: %v", branch, refs)
	case !isUrl(repo):
		localRepo, err := git.PlainOpen(repo)
		if err != nil {
			//return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
			return hash, fmt.Errorf("failed to open local git repository: %w", err)
		}

		headRef, err := localRepo.Head()
		if err != nil {
			//return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
			return hash, fmt.Errorf("failed to get head for local git repo: %v", err)
		}

		hash = headRef.Hash().String()
		if hash == "" {
			//return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
			return hash, fmt.Errorf("failed to retrieve hash for local git repo")
		}

		return hash, err
	}

	return hash, err
}

// CleanUp removes the local directory if it is older than maxAge.
func (g *GitImplementer) CleanUp(directory string, maxAge time.Duration) (err error) {
	tempDir := os.TempDir()

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return fmt.Errorf("failed to read temporary directory: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		if !strings.HasPrefix(entry.Name(), "cdk8s-git-clone-") &&
			!strings.HasPrefix(entry.Name(), "jsii-runtime.") {
			continue
		}

		fullPath := filepath.Join(tempDir, entry.Name())

		if fullPath == directory {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if time.Since(info.ModTime()) > maxAge {
			if err := os.RemoveAll(fullPath); err != nil {
				continue
			}
		}
	}

	return err
}

// isUrl checks if the given string is a valid URL.
func isUrl(repo string) bool {
	if repo == "" {
		return false
	}
	parsedUrl, err := url.ParseRequestURI(repo)
	if err != nil {
		return false
	}

	if parsedUrl.Scheme != "" {
		return true
	} else {
		return false
	}
}

// empty checks if the repo and directory strings are empty.
func empty(repo string, directory string) bool {
	if repo == "" || directory == "" {
		return true
	}
	return false
}
