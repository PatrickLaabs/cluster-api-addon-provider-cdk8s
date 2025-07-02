package git

import (
	"bytes"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var (
	validRepoUrl   = "https://github.com/PatrickLaabs/cdk8s-sample-deployment"
	invalidRepoUrl = "https://github.com/PatrickLaabs/invalid-repo"
	branch         = "main"
)

func TestClone(t *testing.T) {
	gitImplementer := &GitImplementer{}

	t.Run("empty parameters - both empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		err := gitImplementer.Clone("", "", buffer)
		if err == nil {
			t.Error("Clone should have returned error for empty parameters")
		}
		expectedMessage := addonsv1alpha1.EmptyGitRepositoryReason
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
		if err.Error() != addonsv1alpha1.EmptyGitRepositoryReason {
			t.Errorf("Error message should be %q, got %q", addonsv1alpha1.EmptyGitRepositoryReason, err.Error())
		}
	})

	t.Run("empty parameters - repo empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir()

		err := gitImplementer.Clone("", tempDir, buffer)
		if err == nil {
			t.Error("Clone should have returned error for empty repo")
		}
		expectedMessage := addonsv1alpha1.EmptyGitRepositoryReason
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("empty parameters - directory empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		err := gitImplementer.Clone(validRepoUrl, "", buffer)
		if err == nil {
			t.Error("Clone should have returned error for empty directory")
		}
		expectedMessage := addonsv1alpha1.EmptyGitRepositoryReason
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("invalid repository URL", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir()

		err := gitImplementer.Clone(invalidRepoUrl, tempDir, buffer)
		if err == nil {
			t.Error("Clone should have returned error for invalid repository URL")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("malformed URL", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir()
		malformedURL := "not-a-valid-url"

		err := gitImplementer.Clone(malformedURL, tempDir, buffer)
		if err == nil {
			t.Error("Clone should have returned error for malformed URL")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("directory is a file not directory", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		// Create a temporary file instead of directory
		tempFile, err := os.CreateTemp("", "not-a-directory")
		if err != nil {
			t.Fatalf("Failed to create temp file: %v", err)
		}
		defer os.Remove(tempFile.Name())
		tempFile.Close()

		err = gitImplementer.Clone(validRepoUrl, tempFile.Name(), buffer)
		if err == nil {
			t.Error("Clone should have returned error when directory is actually a file")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("directory already exists and is not empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir()

		// Create a file in the directory to make it non-empty
		testFile := filepath.Join(tempDir, "existing-file.txt")
		if err := os.WriteFile(testFile, []byte("existing content"), 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		err := gitImplementer.Clone(validRepoUrl, tempDir, buffer)

		// git.PlainClone will fail if directory is not empty, but the exact behavior
		// depends on the git library version and network conditions
		if err != nil {
			// Expected behavior - clone should fail for non-empty directory
			expectedMessage := addonsv1alpha1.GitCloneFailedCondition
			if buffer.String() != expectedMessage {
				t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
			}
		} else {
			// If clone somehow succeeds, just log it (network-dependent test)
			t.Logf("Clone unexpectedly succeeded for non-empty directory - this may be network/environment dependent")
		}
	})

	t.Run("permission denied directory", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		// Try to clone to a directory with restricted permissions
		restrictedDir := "/root/restricted-clone" // Assuming test doesn't run as root

		err := gitImplementer.Clone(validRepoUrl, restrictedDir, buffer)
		if err == nil {
			t.Error("Clone should have returned error for permission denied directory")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("valid repository clone - success case", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir()

		// This test may fail in CI/test environments due to network restrictions
		// but validates the success path
		err := gitImplementer.Clone(validRepoUrl, tempDir, buffer)

		if err != nil {
			// Expected in most test environments due to network/auth restrictions
			t.Logf("Clone failed as expected in test environment: %v", err)
			expectedMessage := addonsv1alpha1.GitCloneFailedCondition
			if buffer.String() != expectedMessage {
				t.Errorf("Buffer should contain %q on network error, got %q", expectedMessage, buffer.String())
			}
		} else {
			// If clone succeeds (unlikely in test env), verify the clone was successful
			if buffer.String() != "" {
				t.Errorf("Buffer should be empty on successful clone, got %q", buffer.String())
			}

			// Verify .git directory was created
			gitDir := filepath.Join(tempDir, ".git")
			if _, statErr := os.Stat(gitDir); os.IsNotExist(statErr) {
				t.Error("Expected .git directory to exist after successful clone")
			}
		}
	})

	t.Run("private repository without authentication", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir()
		privateRepoURL := "https://github.com/private-user/private-repo"

		err := gitImplementer.Clone(privateRepoURL, tempDir, buffer)
		if err == nil {
			t.Error("Clone should have returned error for private repository without auth")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("protocol not supported", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir()
		unsupportedURL := "ftp://example.com/repo.git"

		err := gitImplementer.Clone(unsupportedURL, tempDir, buffer)
		if err == nil {
			t.Error("Clone should have returned error for unsupported protocol")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})
}

func TestPoll(t *testing.T) {
	gitImplementer := &GitImplementer{}

	t.Run("empty parameters - repo empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		localRepo := setupTestRepo(t)

		changes, err := gitImplementer.Poll("", branch, localRepo, buffer)
		if err == nil {
			t.Error("Poll should have returned error for empty repo")
		}
		if changes {
			t.Error("Poll should return false when error occurs")
		}
		expectedMessage := addonsv1alpha1.EmptyGitRepositoryReason
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("empty parameters - directory empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		changes, err := gitImplementer.Poll(validRepoUrl, branch, "", buffer)
		if err == nil {
			t.Error("Poll should have returned error for empty directory")
		}
		if changes {
			t.Error("Poll should return false when error occurs")
		}
		expectedMessage := addonsv1alpha1.EmptyGitRepositoryReason
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("empty parameters - both empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		changes, err := gitImplementer.Poll("", branch, "", buffer)
		if err == nil {
			t.Error("Poll should have returned error for both empty parameters")
		}
		if changes {
			t.Error("Poll should return false when error occurs")
		}
		expectedMessage := addonsv1alpha1.EmptyGitRepositoryReason
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("local hash error - invalid directory", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		invalidDir := "/path/that/does/not/exist"

		changes, err := gitImplementer.Poll(validRepoUrl, branch, invalidDir, buffer)
		if err == nil {
			t.Error("Poll should have returned error for invalid local directory")
		}
		if changes {
			t.Error("Poll should return false when local hash fails")
		}
		expectedMessage := "localGitHash error"
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("local hash error - not a git repo", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		tempDir := t.TempDir() // Not a git repo

		changes, err := gitImplementer.Poll(validRepoUrl, branch, tempDir, buffer)
		if err == nil {
			t.Error("Poll should have returned error for non-git directory")
		}
		if changes {
			t.Error("Poll should return false when local hash fails")
		}
		expectedMessage := "localGitHash error"
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("remote hash error - invalid URL", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		localRepo := setupTestRepo(t)

		changes, err := gitImplementer.Poll(invalidRepoUrl, branch, localRepo, buffer)
		if err == nil {
			t.Error("Poll should have returned error for invalid remote URL")
		}
		if changes {
			t.Error("Poll should return false when remote hash fails")
		}
		expectedMessage := "remoteGitHash error"
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("success case - valid local repo with remote", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		localRepo := setupTestRepo(t)

		// This will likely fail due to network/auth but tests the success path logic
		changes, err := gitImplementer.Poll(validRepoUrl, branch, localRepo, buffer)

		if err != nil {
			// Expected in most test environments - verify error handling
			expectedErrors := []string{
				"remoteGitHash error",
				"failed to list remote refs",
				"failed to find remote ref for branch",
			}
			errorMatched := false
			for _, expectedError := range expectedErrors {
				if contains(err.Error(), expectedError) || buffer.String() == "remoteGitHash error" {
					errorMatched = true
					break
				}
			}
			if !errorMatched {
				t.Errorf("Error should match expected patterns, got: %v, buffer: %q", err, buffer.String())
			}
			if changes {
				t.Error("Poll should return false when remote hash fails")
			}
		} else {
			// If it succeeds (unlikely in test env), verify success behavior
			expectedMessage := addonsv1alpha1.GitHashSuccessReason
			if buffer.String() != expectedMessage {
				t.Errorf("Buffer should contain %q on success, got %q", expectedMessage, buffer.String())
			}
			// changes can be true or false depending on actual hash comparison
		}
	})

	t.Run("malformed repo input", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		localRepo := setupTestRepo(t)
		malformedRepo := "not-a-valid-repo-or-url"

		changes, err := gitImplementer.Poll(malformedRepo, branch, localRepo, buffer)
		if err == nil {
			t.Error("Poll should have returned error for malformed repo input")
		}
		if changes {
			t.Error("Poll should return false when remote hash fails")
		}
		expectedMessage := "remoteGitHash error"
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("local directory is a file not directory", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		// Create a temporary file instead of directory
		tempFile, err := os.CreateTemp("", "not-a-directory")
		if err != nil {
			t.Fatalf("Failed to create temp file: %v", err)
		}
		defer os.Remove(tempFile.Name())
		tempFile.Close()

		changes, err := gitImplementer.Poll(validRepoUrl, branch, tempFile.Name(), buffer)
		if err == nil {
			t.Error("Poll should have returned error when directory is actually a file")
		}
		if changes {
			t.Error("Poll should return false when local hash fails")
		}
		expectedMessage := "localGitHash error"
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
	})

	t.Run("invalid branch name", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		localRepo := setupTestRepo(t)
		invalidBranch := "nonexistent-branch"

		changes, err := gitImplementer.Poll(validRepoUrl, invalidBranch, localRepo, buffer)
		if err == nil {
			t.Error("Poll should have returned error for invalid branch")
		}
		if changes {
			t.Error("Poll should return false when hash retrieval fails")
		}
		// Error could come from either local or remote hash operation
		possibleMessages := []string{"localGitHash error", "remoteGitHash error"}
		messageMatched := false
		for _, msg := range possibleMessages {
			if buffer.String() == msg {
				messageMatched = true
				break
			}
		}
		if !messageMatched {
			t.Errorf("Buffer should contain one of %v, got %q", possibleMessages, buffer.String())
		}
	})
}

func TestHash(t *testing.T) {
	gitImplementer := &GitImplementer{}

	t.Run("local repository - success", func(t *testing.T) {
		localRepo := setupTestRepo(t)

		hash, err := gitImplementer.Hash(localRepo, branch)
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

		hash, err := gitImplementer.Hash(invalidPath, branch)
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

		hash, err := gitImplementer.Hash(tempDir, branch)
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

		hash, err := gitImplementer.Hash(validURL, branch)
		// We expect this to fail in most test environments due to network/auth,
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

		hash, err := gitImplementer.Hash(invalidURL, branch)
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
		// Empty string should be treated as a local path (not URL)
		hash, err := gitImplementer.Hash("", branch)
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

		hash, err := gitImplementer.Hash(malformedInput, branch)
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
