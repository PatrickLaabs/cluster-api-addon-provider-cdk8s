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

	t.Run("empty parameters - repo empty", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		directory, err := gitImplementer.Clone("", buffer)
		if err == nil {
			t.Error("Clone should have returned error for empty repo")
		}
		// Directory should still be created even on error
		if directory == "" {
			t.Error("Directory should be returned even on error")
		}
		expectedMessage := addonsv1alpha1.EmptyGitRepositoryReason
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
		// Clean up created directory
		if directory != "" {
			os.RemoveAll(directory)
		}
	})

	t.Run("invalid repository URL", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		directory, err := gitImplementer.Clone(invalidRepoUrl, buffer)
		if err == nil {
			t.Error("Clone should have returned error for invalid repository URL")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
		// Clean up created directory
		if directory != "" {
			os.RemoveAll(directory)
		}
	})

	t.Run("malformed URL", func(t *testing.T) {
		buffer := &bytes.Buffer{}
		malformedURL := "not-a-valid-url"

		directory, err := gitImplementer.Clone(malformedURL, buffer)
		if err == nil {
			t.Error("Clone should have returned error for malformed URL")
		}
		expectedMessage := addonsv1alpha1.GitCloneFailedCondition
		if buffer.String() != expectedMessage {
			t.Errorf("Buffer should contain %q, got %q", expectedMessage, buffer.String())
		}
		// Clean up created directory
		if directory != "" {
			os.RemoveAll(directory)
		}
	})

	t.Run("valid repository clone - success case", func(t *testing.T) {
		buffer := &bytes.Buffer{}

		directory, err := gitImplementer.Clone(validRepoUrl, buffer)

		if err != nil {
			// Expected in most test environments due to network/auth restrictions
			t.Logf("Clone failed as expected in test environment: %v", err)
			expectedMessage := addonsv1alpha1.GitCloneFailedCondition
			if buffer.String() != expectedMessage {
				t.Errorf("Buffer should contain %q on network error, got %q", expectedMessage, buffer.String())
			}
		} else {
			// If clone succeeds, verify the clone was successful
			if buffer.String() != "" {
				t.Errorf("Buffer should be empty on successful clone, got %q", buffer.String())
			}

			// Verify .git directory was created in returned directory
			gitDir := filepath.Join(directory, ".git")
			if _, statErr := os.Stat(gitDir); os.IsNotExist(statErr) {
				t.Error("Expected .git directory to exist after successful clone")
			}
		}

		// Clean up created directory
		if directory != "" {
			os.RemoveAll(directory)
		}
	})

	t.Run("nil buffer parameter", func(t *testing.T) {
		// This should not panic even with nil buffer
		directory, err := gitImplementer.Clone(validRepoUrl, nil)

		if err != nil {
			// This is expected in test environments due to network restrictions
			t.Logf("Clone failed as expected in test environment: %v", err)
		} else {
			// If clone succeeds, verify the .git directory was created
			gitDir := filepath.Join(directory, ".git")
			if _, err := os.Stat(gitDir); os.IsNotExist(err) {
				t.Error("Expected .git directory to exist after successful clone")
			}
		}

		// Most importantly, the method should not have panicked
		t.Log("Method completed without panic despite nil buffer")

		// Clean up created directory
		if directory != "" {
			os.RemoveAll(directory)
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

func TestCleanUp(t *testing.T) {
	gitImplementer := &GitImplementer{}

	t.Run("cleanup old directories but preserve current", func(t *testing.T) {
		// Create old directories that should be cleaned up
		oldDir1, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}

		oldDir2, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}

		// Create current directory (should be preserved)
		currentDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}

		// Age the old directories
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(oldDir1, oldTime, oldTime); err != nil {
			t.Fatalf("Failed to change times for old dir 1: %v", err)
		}
		if err := os.Chtimes(oldDir2, oldTime, oldTime); err != nil {
			t.Fatalf("Failed to change times for old dir 2: %v", err)
		}

		// Run cleanup with 1 hour max age
		err = gitImplementer.CleanUp(currentDir, 1*time.Hour)
		if err != nil {
			t.Errorf("CleanUp returned error: %v", err)
		}

		// Verify old directories were removed
		if _, err := os.Stat(oldDir1); !os.IsNotExist(err) {
			t.Error("Old directory 1 should have been removed")
			os.RemoveAll(oldDir1) // cleanup
		}

		if _, err := os.Stat(oldDir2); !os.IsNotExist(err) {
			t.Error("Old directory 2 should have been removed")
			os.RemoveAll(oldDir2) // cleanup
		}

		// Verify current directory is preserved
		if _, err := os.Stat(currentDir); os.IsNotExist(err) {
			t.Error("Current directory should not have been removed")
		} else {
			os.RemoveAll(currentDir) // cleanup
		}
	})

	t.Run("respects maxAge parameter", func(t *testing.T) {
		// Create recent directory that shouldn't be cleaned
		recentDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(recentDir)

		currentDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(currentDir)

		// Age the recent directory only slightly
		recentTime := time.Now().Add(-30 * time.Minute)
		if err := os.Chtimes(recentDir, recentTime, recentTime); err != nil {
			t.Fatalf("Failed to change times for recent dir: %v", err)
		}

		// Run cleanup with 1 hour max age
		err = gitImplementer.CleanUp(currentDir, 1*time.Hour)
		if err != nil {
			t.Errorf("CleanUp returned error: %v", err)
		}

		// Recent directory should still exist
		if _, err := os.Stat(recentDir); os.IsNotExist(err) {
			t.Error("Recent directory should not have been removed")
		}
	})

	t.Run("ignores non-matching directories", func(t *testing.T) {
		// Create directory with different prefix
		otherDir, err := os.MkdirTemp("", "other-prefix-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(otherDir)

		currentDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(currentDir)

		// Age the other directory
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(otherDir, oldTime, oldTime); err != nil {
			t.Fatalf("Failed to change times for other dir: %v", err)
		}

		// Run cleanup
		err = gitImplementer.CleanUp(currentDir, 1*time.Hour)
		if err != nil {
			t.Errorf("CleanUp returned error: %v", err)
		}

		// Other directory should still exist (different prefix)
		if _, err := os.Stat(otherDir); os.IsNotExist(err) {
			t.Error("Directory with different prefix should not have been removed")
		}
	})

	t.Run("handles empty current directory parameter", func(t *testing.T) {
		// Create old directory
		oldDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}

		// Age the directory
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(oldDir, oldTime, oldTime); err != nil {
			t.Fatalf("Failed to change times for old dir: %v", err)
		}

		// Run cleanup with empty current directory
		err = gitImplementer.CleanUp("", 1*time.Hour)
		if err != nil {
			t.Errorf("CleanUp returned error: %v", err)
		}

		// Old directory should be removed since no current directory to preserve
		if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
			t.Error("Old directory should have been removed when no current directory specified")
			os.RemoveAll(oldDir) // cleanup
		}
	})

	t.Run("cleanup with zero max age removes all old directories", func(t *testing.T) {
		// Create multiple directories
		dirs := make([]string, 3)
		for i := 0; i < 3; i++ {
			dir, err := os.MkdirTemp("", "cdk8s-git-clone-")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			dirs[i] = dir
		}

		currentDir := dirs[2] // Last one is current

		// Age the first two directories slightly
		oldTime := time.Now().Add(-1 * time.Second)
		if err := os.Chtimes(dirs[0], oldTime, oldTime); err != nil {
			t.Fatalf("Failed to change times for dir 0: %v", err)
		}
		if err := os.Chtimes(dirs[1], oldTime, oldTime); err != nil {
			t.Fatalf("Failed to change times for dir 1: %v", err)
		}

		// Run cleanup with zero max age
		err := gitImplementer.CleanUp(currentDir, 0)
		if err != nil {
			t.Errorf("CleanUp returned error: %v", err)
		}

		// First two should be removed
		for i := 0; i < 2; i++ {
			if _, err := os.Stat(dirs[i]); !os.IsNotExist(err) {
				t.Errorf("Directory %d should have been removed", i)
				os.RemoveAll(dirs[i])
			}
		}

		// Current should remain
		if _, err := os.Stat(currentDir); os.IsNotExist(err) {
			t.Error("Current directory should not have been removed")
		} else {
			os.RemoveAll(currentDir)
		}
	})

	t.Run("handles directories with files inside", func(t *testing.T) {
		// Create directory with files (simulating actual git clone)
		oldDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}

		// Create some files inside
		testFile := filepath.Join(oldDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		// Create subdirectory
		subDir := filepath.Join(oldDir, "subdir")
		if err := os.Mkdir(subDir, 0755); err != nil {
			t.Fatalf("Failed to create subdirectory: %v", err)
		}

		currentDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(currentDir)

		// Age the old directory
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(oldDir, oldTime, oldTime); err != nil {
			t.Fatalf("Failed to change times for old dir: %v", err)
		}

		// Run cleanup
		err = gitImplementer.CleanUp(currentDir, 1*time.Hour)
		if err != nil {
			t.Errorf("CleanUp returned error: %v", err)
		}

		// Directory and all contents should be removed
		if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
			t.Error("Directory with contents should have been removed")
			os.RemoveAll(oldDir) // cleanup
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

// Helper functions.
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
