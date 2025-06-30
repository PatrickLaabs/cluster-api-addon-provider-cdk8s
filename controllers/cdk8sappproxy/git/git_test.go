package git

import (
	"bytes"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"os"
	"testing"
	"time"
)

var (
	validRepoUrl   = "https://github.com/PatrickLaabs/cdk8s-sample-deployment"
	invalidRepoUrl = "https://github.com/PatrickLaabs/invalid-repo"
	incorrectUri   = "testy"
)

func setupTestRepo(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()

	t.Cleanup(func() {
		err := os.RemoveAll(tempDir)
		if err != nil {
			t.Errorf("Cleaning up temp dir failed: %v", err)
		}
	})

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	fileName := tempDir + "/cdk8s-sample-deployment.yaml"
	if err := os.WriteFile(fileName, []byte("yaml-content"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	_, err = w.Add("cdk8s-sample-deployment.yaml")
	if err != nil {
		t.Fatalf("failed to add cdk8s-sample-deployment.yaml: %v", err)
	}

	_, err = w.Commit("cdk8s-sample-deployment.yaml", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Tester",
			Email: "tester@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	return tempDir
}

func TestGitOperations(t *testing.T) {
	checkClone := func(tb testing.TB, gitOperator GitOperator, repoUrl string, wantErr bool, wantMessage string, currentCommitHash string) {
		tb.Helper()
		tempDir := t.TempDir()
		buffer := bytes.Buffer{}

		currentCommitHash, err := gitOperator.Clone(repoUrl, tempDir, &buffer)
		if wantErr && err == nil {
			t.Errorf("git clone should have failed")
		}
		if !wantErr && err != nil {
			t.Errorf("git clone should have succeeded")
		}

		if wantErr {
			if _, statErr := os.Stat(tempDir); os.IsNotExist(statErr) {
				t.Errorf("expected .git directory to exist after clone, but it does not")
			}
		}

		gotMessage := buffer.String()

		if gotMessage != wantMessage {
			t.Errorf("got %q, want %q", gotMessage, wantMessage)
		}

		t.Cleanup(func() {
			err := os.RemoveAll(tempDir)
			if err != nil {
				t.Errorf("Cleaning up temp dir failed: %v", err)
			}
		})
	}

	t.Run("success-clone-repo", func(t *testing.T) {
		gitImplementer := &GitImplementer{}
		checkClone(t, gitImplementer, validRepoUrl, false, addonsv1alpha1.GitCloneSuccessCondition, "0")
	})

	t.Run("failure-clone-repo", func(t *testing.T) {
		gitImplementer := &GitImplementer{}
		checkClone(t, gitImplementer, invalidRepoUrl, true, addonsv1alpha1.GitCloneFailedCondition, "0")
	})
}

func TestGitPoller(t *testing.T) {
	gitImplementer := &GitImplementer{}
	localRepo := setupTestRepo(t)

	t.Run("polling remote repo failed", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		_, err := gitImplementer.Poll(invalidRepoUrl, "main", localRepo, buffer)
		if err == nil {
			t.Errorf("expected error for invalid remote repo")
		}
	})
	t.Run("polling local repo failed", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		_, err := gitImplementer.Poll(validRepoUrl, "main", "/invalid/local/path", buffer)
		if err == nil {
			t.Errorf("expected error for invalid local repo")
		}
	})
	t.Run("polling detects changes", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		detected, err := gitImplementer.Poll(validRepoUrl, "main", localRepo, buffer)
		if err != nil {
			t.Errorf("change detection should have failed: %v", err)
		}
		if !detected {
			t.Errorf("change detection should be successful: %v", err)
		}
	})
}

//func TestGitPoller(t *testing.T) {
//	checkPoller := func(tb testing.TB, gitOperator GitOperator, repoUrl string, branch string, directory string, wantChangeDetection bool, wantErr bool, wantMessage string) {
//		tb.Helper()
//		buffer := bytes.Buffer{}
//
//		detectedChanges, err := gitOperator.Poll(repoUrl, branch, directory, &buffer)
//		if wantErr && err == nil {
//			t.Errorf("git poll should have failed")
//		}
//		if !wantErr && err != nil {
//			t.Errorf("git poll should have succeeded")
//		}
//
//		gotMessage := buffer.String()
//
//		if gotMessage != wantMessage {
//			t.Errorf("got %q, want %q", gotMessage, wantMessage)
//		}
//
//		if detectedChanges != wantChangeDetection {
//			t.Errorf("got %v, want %v", detectedChanges, wantChangeDetection)
//		}
//	}
//	t.Run("polling remote repo failed", func(t *testing.T) {
//		gitImplementer := &GitImplementer{}
//		checkPoller(t, gitImplementer, invalidRepoUrl, "main", validRepoUrl, false, true, addonsv1alpha1.GitHashFailureReason)
//	})
//	t.Run("polling remote repo succeeded", func(t *testing.T) {
//		gitImplementer := &GitImplementer{}
//		checkPoller(t, gitImplementer, validRepoUrl, "main", validRepoUrl, false, false, addonsv1alpha1.GitHashSuccessReason)
//	})
//	t.Run("polling local repo failed", func(t *testing.T) {
//		gitImplementer := &GitImplementer{}
//		localRepo := setupTestRepo(t)
//		checkPoller(t, gitImplementer, validRepoUrl, "main", localRepo, false, true, addonsv1alpha1.GitHashFailureReason)
//	})
//	t.Run("polling local repo succeeded", func(t *testing.T) {
//		gitImplementer := &GitImplementer{}
//		localRepo := setupTestRepo(t)
//		remoteRepo := "https://github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s"
//		checkPoller(t, gitImplementer, remoteRepo, "main", localRepo, false, false, addonsv1alpha1.GitHashSuccessReason)
//	})
//}

func TestValidateRepoUrl(t *testing.T) {
	checkValidateRepoUrl := func(tb testing.TB, repoUrl string, wantErr bool, wantMessage string) {
		tb.Helper()
		buffer := bytes.Buffer{}

		err := validateRepoUrl(repoUrl, &buffer)
		if wantErr && err == nil {
			t.Errorf("validateRepoUrl should have failed")
		}
		if !wantErr && err != nil {
			t.Errorf("validateRepoUrl should have succeeded")
		}

		gotMessage := buffer.String()

		if gotMessage != wantMessage {
			t.Errorf("got %q, want %q", gotMessage, wantMessage)
		}
	}
	t.Run("empty repo", func(t *testing.T) {
		checkValidateRepoUrl(t, "", true, addonsv1alpha1.EmptyGitRepositoryReason)
	})
	//t.Run("valid repo", func(t *testing.T) {
	//	checkValidateRepoUrl(t, validRepoUrl, false, addonsv1alpha1.ValidGitRepositoryReason)
	//})
	t.Run("invalid repo", func(t *testing.T) {
		checkValidateRepoUrl(t, incorrectUri, true, addonsv1alpha1.InvalidGitRepositoryReason)
	})
}

func TestLocalGitHash(t *testing.T) {
	checkLocalGitHash := func(tb testing.TB, repoUrl string, wantErr bool, wantMessage string) {
		tb.Helper()
		buffer := bytes.Buffer{}

		_, err := localGitHash(repoUrl, &buffer)
		if wantErr && err == nil {
			t.Errorf("fetchCurrentCommit should have failed")
		}
		if !wantErr && err != nil {
			t.Errorf("fetchCurrentCommit should have succeeded")
		}
		gotMessage := buffer.String()

		if gotMessage != wantMessage {
			t.Errorf("got %q, want %q", gotMessage, wantMessage)
		}
	}
	t.Run("success commitHash", func(t *testing.T) {
		testRepourl := setupTestRepo(t)
		checkLocalGitHash(t, testRepourl, false, "")
	})
	t.Run("failure commitHash", func(t *testing.T) {
		checkLocalGitHash(t, invalidRepoUrl, true, "")
	})
}

func TestRemoteGitHash(t *testing.T) {
	checkRemoteGitHash := func(tb testing.TB, repoUrl string, branch string, wantErr bool, wantMessage string) {
		tb.Helper()
		buffer := bytes.Buffer{}

		_, err := remoteGitHash(repoUrl, branch, &buffer)
		if wantErr {
			if err == nil {
				t.Errorf("checking remote git hash should have failed")
			} else if err.Error() != wantMessage {
				t.Errorf("got error %q, want %q", err.Error(), wantMessage)
			}
		} else {
			if err != nil {
				t.Errorf("checking remote git hash should have succeeded")
			}
			gotMessage := buffer.String()
			if gotMessage != wantMessage {
				t.Errorf("got %q, want %q", gotMessage, wantMessage)
			}
		}
	}
	t.Run("success commitHash", func(t *testing.T) {
		checkRemoteGitHash(t, validRepoUrl, "main", false, "")
	})
	t.Run("failure commitHash", func(t *testing.T) {
		checkRemoteGitHash(t, validRepoUrl, "main", true, addonsv1alpha1.EmptyGitRepositoryReason)
	})
}
