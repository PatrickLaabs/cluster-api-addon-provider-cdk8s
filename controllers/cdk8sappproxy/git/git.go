package git

import (
	"bytes"
	"fmt"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"net/url"
)

type GitOperator interface {
	Clone(repoUrl string, directory string, writer *bytes.Buffer) (currentCommitHash string, err error)
	Poll(repoUrl string, branch string, directory string, writer *bytes.Buffer) (detectedChanges bool, err error)
}

type GitImplementer struct{}

// Clone clones the given repository to a local directory.
func (g *GitImplementer) Clone(repoUrl string, directory string, writer *bytes.Buffer) (currentCommitHash string, err error) {
	validationBuffer := new(bytes.Buffer)
	currentCommitHash = "0"

	err = validateRepoUrl(repoUrl, validationBuffer)
	if err != nil {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.InvalidGitRepositoryReason)

		return currentCommitHash, err
	}

	_, err = git.PlainClone(directory, false, &git.CloneOptions{
		URL: repoUrl,
	})
	if err != nil {
		fmt.Fprintf(writer, addonsv1alpha1.GitCloneFailedCondition)

		return currentCommitHash, err
	}

	localGitHashBuffer := new(bytes.Buffer)
	currentCommitHash, err = localGitHash(directory, localGitHashBuffer)
	if err != nil {
		fmt.Fprintf(writer, addonsv1alpha1.ClusterSelectorParseFailedReason)

		return currentCommitHash, err
	}

	fmt.Fprintf(writer, addonsv1alpha1.GitCloneSuccessCondition)

	return currentCommitHash, err
}

// Poll polls for changes for the given remote git repository.
// returns true, if current local commit hash and remote hash are not equal.
func (g *GitImplementer) Poll(repoUrl string, branch string, directory string, writer *bytes.Buffer) (detectedChanges bool, err error) {
	// Defaults to false. We only change to true if there is a difference between the hashes.
	detectedChanges = false

	err = validateRepoUrl(repoUrl, writer)
	if err != nil {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.GitHashFailureReason)

		return detectedChanges, err
	}
	if directory == "" {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.GitHashFailureReason)

		return detectedChanges, err
	}

	// get hash from local repo
	localHash, err := localGitHash(directory, writer)
	if err != nil {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.GitHashFailureReason)

		return detectedChanges, err
	}

	// Get Hash from remote repo
	remoteHash, err := remoteGitHash(repoUrl, branch, writer)
	if err != nil {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.GitHashFailureReason)

		return detectedChanges, err
	}

	if localHash != remoteHash {
		detectedChanges = true
	}

	fmt.Fprintf(writer, "%s", addonsv1alpha1.GitHashSuccessReason)

	return detectedChanges, err
}

// validateRepoUrl validates the given repoUrl.
func validateRepoUrl(repoUrl string, writer *bytes.Buffer) (err error) {
	// Checking, if repoUrl is empty
	if repoUrl == "" {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.EmptyGitRepositoryReason)

		return fmt.Errorf("%s", addonsv1alpha1.EmptyGitRepositoryReason)
	}

	_, err = url.ParseRequestURI(repoUrl)
	if err != nil {
		fmt.Fprintf(writer, "%s", addonsv1alpha1.InvalidGitRepositoryReason)

		return fmt.Errorf("%s", addonsv1alpha1.InvalidGitRepositoryReason)
	}

	return err
}

// localGitHash checks the current git hash for the given local repository.
func localGitHash(directory string, writer *bytes.Buffer) (hash string, err error) {
	hash = "0"
	repo, err := git.PlainOpen(directory)
	if err != nil {
		return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
	}

	headRef, err := repo.Head()
	if err != nil {
		return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
	}

	hash = headRef.Hash().String()
	if hash == "" {
		return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
	}

	return hash, err
}

// remoteGitHash checks the git hash for the given remote repository.
func remoteGitHash(repoUrl string, branch string, writer *bytes.Buffer) (hash string, err error) {
	// validates the given remote repository url
	err = validateRepoUrl(repoUrl, nil)
	if err != nil {
		return hash, err
	}

	repo := git.NewRemote(nil, &config.RemoteConfig{
		URLs: []string{repoUrl},
		Name: "origin",
	})

	refs, err := repo.List(&git.ListOptions{})
	if err != nil {
		return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
	}

	refName := plumbing.NewBranchReferenceName(branch)
	for _, ref := range refs {
		if ref.Name() == refName {
			return ref.Hash().String(), nil
		}
	}

	return hash, fmt.Errorf("%s", addonsv1alpha1.GitHashFailureReason)
}
