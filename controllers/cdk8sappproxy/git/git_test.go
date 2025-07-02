package git

import (
	"bytes"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"net/url"
	"os"
	"testing"
	"time"
)

var (
	validRepoUrl   = "https://github.com/PatrickLaabs/cdk8s-sample-deployment"
	invalidRepoUrl = "https://github.com/PatrickLaabs/invalid-repo"
	incorrectUri   = "testy"
)

func TestGitOperations(t *testing.T) {
	checkClone := func(tb testing.TB, gitOperator GitOperator, repoUrl string, wantErr bool, wantMessage string, currentCommitHash string) {
		tb.Helper()
		tempDir := t.TempDir()
		buffer := bytes.Buffer{}

		err := gitOperator.Clone(repoUrl, tempDir, &buffer)
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

func TestHash(t *testing.T) {
	gitImplementer := &GitImplementer{}

	t.Run("local repository - success", func(t *testing.T) {
		localRepo := setupTestRepo(t)

		hash, err := gitImplementer.Hash(localRepo)
		if err != nil {
			t.Errorf("Hash(%q) returned error: %v", localRepo, err)
		}
		if hash == "" {
			t.Error("Hash should return non-empty hash for valid local repo")
		}
		// Git hashes are 40 character hex strings
		if len(hash) != 40 {
			t.Errorf("Hash length = %d, expected 40", len(hash))
		}
	})

	t.Run("local repository - invalid path", func(t *testing.T) {
		invalidPath := "/path/that/does/not/exist"

		hash, err := gitImplementer.Hash(invalidPath)
		if err == nil {
			t.Errorf("Hash(%q) should have returned error for invalid path", invalidPath)
		}
		if hash != "" {
			t.Errorf("Hash should return empty string on error, got %q", hash)
		}
		expectedError := "failed to open local git repository"
		if !contains(err.Error(), expectedError) {
			t.Errorf("Error message should contain %q, got %q", expectedError, err.Error())
		}
	})

	t.Run("local repository - not a git repo", func(t *testing.T) {
		// Create a temp directory that's not a git repo
		tempDir := t.TempDir()

		hash, err := gitImplementer.Hash(tempDir)
		if err == nil {
			t.Errorf("Hash(%q) should have returned error for non-git directory", tempDir)
		}
		if hash != "" {
			t.Errorf("Hash should return empty string on error, got %q", hash)
		}
		expectedError := "failed to open local git repository"
		if !contains(err.Error(), expectedError) {
			t.Errorf("Error message should contain %q, got %q", expectedError, err.Error())
		}
	})

	t.Run("remote repository - valid URL format", func(t *testing.T) {
		// This test will likely fail due to network/auth requirements
		// but tests the URL detection logic
		validURL := "https://github.com/PatrickLaabs/cdk8s-sample-deployment"

		hash, err := gitImplementer.Hash(validURL)
		// We expect this to fail in most test environments due to network/auth
		// but we can verify the error handling
		if err != nil {
			expectedErrors := []string{
				"failed to list remote refs",
				"failed to find remote ref for branch main",
			}
			errorContainsExpected := false
			for _, expectedError := range expectedErrors {
				if contains(err.Error(), expectedError) {
					errorContainsExpected = true
					break
				}
			}
			if !errorContainsExpected {
				t.Errorf("Error should contain one of expected messages, got: %v", err)
			}
		}
		// If it succeeds (unlikely in test env), hash should be non-empty
		if err == nil && hash == "" {
			t.Error("Hash should return non-empty hash for successful remote operation")
		}
	})

	t.Run("remote repository - invalid URL", func(t *testing.T) {
		invalidURL := "https://github.com/invalid/nonexistent-repo"

		hash, err := gitImplementer.Hash(invalidURL)
		if err == nil {
			t.Errorf("Hash(%q) should have returned error for invalid remote repo", invalidURL)
		}
		if hash != "" {
			t.Errorf("Hash should return empty string on error, got %q", hash)
		}
		expectedError := "failed to list remote refs"
		if !contains(err.Error(), expectedError) {
			t.Errorf("Error message should contain %q, got %q", expectedError, err.Error())
		}
	})

	t.Run("empty string input", func(t *testing.T) {
		// Empty string should be treated as local path (not URL)
		hash, err := gitImplementer.Hash("")
		if err == nil {
			t.Error("Hash(\"\") should have returned error for empty string")
		}
		if hash != "" {
			t.Errorf("Hash should return empty string on error, got %q", hash)
		}
		expectedError := "failed to open local git repository"
		if !contains(err.Error(), expectedError) {
			t.Errorf("Error message should contain %q, got %q", expectedError, err.Error())
		}
	})

	t.Run("malformed input", func(t *testing.T) {
		malformedInput := "not-a-url-or-path"

		hash, err := gitImplementer.Hash(malformedInput)
		if err == nil {
			t.Errorf("Hash(%q) should have returned error for malformed input", malformedInput)
		}
		if hash != "" {
			t.Errorf("Hash should return empty string on error, got %q", hash)
		}
		expectedError := "failed to open local git repository"
		if !contains(err.Error(), expectedError) {
			t.Errorf("Error message should contain %q, got %q", expectedError, err.Error())
		}
	})
}

func TestIsUrl(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expected       bool
		expectedScheme string
	}{
		// Valid URLs (should return true)
		{
			name:           "https URL",
			input:          "https://github.com/user/repo",
			expected:       true,
			expectedScheme: "https",
		},
		{
			name:           "http URL",
			input:          "http://example.com/repo",
			expected:       true,
			expectedScheme: "http",
		},
		{
			name:           "git URL",
			input:          "git://github.com/user/repo.git",
			expected:       true,
			expectedScheme: "git",
		},
		{
			name:           "ssh URL",
			input:          "ssh://git@github.com/user/repo.git",
			expected:       true,
			expectedScheme: "ssh",
		},
		{
			name:           "git+ssh URL",
			input:          "git+ssh://git@github.com/user/repo.git",
			expected:       true,
			expectedScheme: "git+ssh",
		},

		// Directory paths (should return false)
		{
			name:           "absolute path",
			input:          "/tmp/local-repo",
			expected:       false,
			expectedScheme: "", // No scheme for paths
		},
		{
			name:           "relative path with dot",
			input:          "./local-repo",
			expected:       false,
			expectedScheme: "",
		},
		{
			name:           "relative path with double dot",
			input:          "../local-repo",
			expected:       false,
			expectedScheme: "",
		},
		{
			name:           "simple directory name",
			input:          "local-repo",
			expected:       false,
			expectedScheme: "",
		},
		{
			name:           "nested path",
			input:          "path/to/local-repo",
			expected:       false,
			expectedScheme: "",
		},
		{
			name:           "temp directory pattern",
			input:          "/tmp/cdk8s-git-clone-123",
			expected:       false,
			expectedScheme: "",
		},

		// Edge cases
		{
			name:           "empty string",
			input:          "",
			expected:       false,
			expectedScheme: "",
		},
		{
			name:           "just scheme",
			input:          "https://",
			expected:       true,
			expectedScheme: "https",
		},
		{
			name:           "malformed URL",
			input:          "not-a-url",
			expected:       false,
			expectedScheme: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isUrl(tt.input)
			if result != tt.expected {
				t.Errorf("isUrl(%q) = %v, expected %v", tt.input, result, tt.expected)
			}

			// Additional test: verify the actual scheme parsing
			if tt.expected || tt.expectedScheme == "" {
				parsedUrl, err := url.ParseRequestURI(tt.input)
				if err != nil && tt.expected {
					t.Errorf("Expected %q to parse successfully, but got error: %v", tt.input, err)
				} else if err == nil {
					if parsedUrl.Scheme != tt.expectedScheme {
						t.Errorf("For input %q, expected scheme %q, but got %q", tt.input, tt.expectedScheme, parsedUrl.Scheme)
					}
				}
			}
		})
	}
}

func TestEmptyChecker(t *testing.T) {
	tests := []struct {
		name      string
		repo      string
		directory string
		expected  bool
	}{
		// Both empty cases
		{
			name:      "both repo and directory empty",
			repo:      "",
			directory: "",
			expected:  true,
		},
		// Single empty cases
		{
			name:      "repo empty, directory not empty",
			repo:      "",
			directory: "/tmp/some-dir",
			expected:  true,
		},
		{
			name:      "repo not empty, directory empty",
			repo:      "https://github.com/user/repo",
			directory: "",
			expected:  true,
		},
		// Both non-empty cases
		{
			name:      "both repo and directory not empty",
			repo:      "https://github.com/user/repo",
			directory: "/tmp/some-dir",
			expected:  false,
		},
		{
			name:      "both repo and directory with local paths",
			repo:      "./local-repo",
			directory: "./target-dir",
			expected:  false,
		},
		// Edge cases with whitespace
		{
			name:      "repo with spaces, directory empty",
			repo:      "   ",
			directory: "",
			expected:  true,
		},
		{
			name:      "repo empty, directory with spaces",
			repo:      "",
			directory: "   ",
			expected:  true,
		},
		{
			name:      "both with spaces (non-empty)",
			repo:      "   ",
			directory: "   ",
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := empty(tt.repo, tt.directory)
			if result != tt.expected {
				t.Errorf("emptyChecker(%q, %q) = %v, expected %v", tt.repo, tt.directory, result, tt.expected)
			}
		})
	}
}

// Helper functions
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

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			func() bool {
				for i := 0; i <= len(s)-len(substr); i++ {
					if s[i:i+len(substr)] == substr {
						return true
					}
				}
				return false
			}())))
}
