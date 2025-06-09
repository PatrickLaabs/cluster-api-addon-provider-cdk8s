package v1alpha1

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestCdk8sAppProxy_Default(t *testing.T) {
	tests := []struct {
		name     string
		proxy    *Cdk8sAppProxy
		expected *Cdk8sAppProxy
	}{
		{
			name: "sets default reference and path for git repository",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL: "https://github.com/example/repo",
					},
				},
			},
			expected: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL:       "https://github.com/example/repo",
						Reference: "main",
						Path:      ".",
					},
				},
			},
		},
		{
			name: "does not override existing reference and path",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL:       "https://github.com/example/repo",
						Reference: "develop",
						Path:      "apps",
					},
				},
			},
			expected: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL:       "https://github.com/example/repo",
						Reference: "develop",
						Path:      "apps",
					},
				},
			},
		},
		{
			name: "handles nil git repository",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					LocalPath: "/local/path",
				},
			},
			expected: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					LocalPath: "/local/path",
				},
			},
		},
		{
			name: "sets default path when reference is provided",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL:       "https://github.com/example/repo",
						Reference: "v1.0.0",
					},
				},
			},
			expected: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL:       "https://github.com/example/repo",
						Reference: "v1.0.0",
						Path:      ".",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			err := tt.proxy.Default(ctx, tt.proxy)
			if err != nil {
				t.Errorf("Default() error = %v", err)
				return
			}

			if tt.proxy.Spec.GitRepository != nil && tt.expected.Spec.GitRepository != nil {
				if tt.proxy.Spec.GitRepository.Reference != tt.expected.Spec.GitRepository.Reference {
					t.Errorf("Expected reference %s, got %s", tt.expected.Spec.GitRepository.Reference, tt.proxy.Spec.GitRepository.Reference)
				}
				if tt.proxy.Spec.GitRepository.Path != tt.expected.Spec.GitRepository.Path {
					t.Errorf("Expected path %s, got %s", tt.expected.Spec.GitRepository.Path, tt.proxy.Spec.GitRepository.Path)
				}
			}

			if tt.proxy.Spec.LocalPath != tt.expected.Spec.LocalPath {
				t.Errorf("Expected LocalPath %s, got %s", tt.expected.Spec.LocalPath, tt.proxy.Spec.LocalPath)
			}
		})
	}
}

func TestCdk8sAppProxy_ValidateCreate(t *testing.T) {
	tests := []struct {
		name      string
		proxy     *Cdk8sAppProxy
		wantError bool
		errorMsg  string
	}{
		{
			name: "valid git repository configuration",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL: "https://github.com/example/repo",
					},
				},
			},
			wantError: false,
		},
		{
			name: "valid local path configuration",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					LocalPath: "/local/path",
				},
			},
			wantError: false,
		},
		{
			name: "invalid - both localPath and gitRepository specified",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					LocalPath: "/local/path",
					GitRepository: &GitRepositorySpec{
						URL: "https://github.com/example/repo",
					},
				},
			},
			wantError: true,
			errorMsg:  "only one of localPath or gitRepository can be specified",
		},
		{
			name: "invalid - neither localPath nor gitRepository specified",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{},
			},
			wantError: true,
			errorMsg:  "either localPath or gitRepository must be specified",
		},
		{
			name: "invalid - gitRepository without URL",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						Reference: "main",
					},
				},
			},
			wantError: true,
			errorMsg:  "gitRepository.url is required when gitRepository is specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			warnings, err := tt.proxy.ValidateCreate(ctx, tt.proxy)

			if tt.wantError {
				if err == nil {
					t.Errorf("ValidateCreate() expected error, got nil")
					return
				}
				if tt.errorMsg != "" && err.Error() != "validation failed: ["+tt.errorMsg+"]" {
					t.Errorf("ValidateCreate() error = %v, want error containing %v", err.Error(), tt.errorMsg)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateCreate() unexpected error = %v", err)
				}
			}

			// Warnings should always be nil in our implementation
			if warnings != nil {
				t.Errorf("ValidateCreate() warnings = %v, want nil", warnings)
			}
		})
	}
}

func TestCdk8sAppProxy_ValidateUpdate(t *testing.T) {
	tests := []struct {
		name      string
		oldProxy  *Cdk8sAppProxy
		newProxy  *Cdk8sAppProxy
		wantError bool
	}{
		{
			name: "valid update from git to local path",
			oldProxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL: "https://github.com/example/repo",
					},
				},
			},
			newProxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					LocalPath: "/local/path",
				},
			},
			wantError: false,
		},
		{
			name: "invalid update - both specified",
			oldProxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL: "https://github.com/example/repo",
					},
				},
			},
			newProxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					LocalPath: "/local/path",
					GitRepository: &GitRepositorySpec{
						URL: "https://github.com/example/repo",
					},
				},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			warnings, err := tt.newProxy.ValidateUpdate(ctx, tt.oldProxy, tt.newProxy)

			if tt.wantError {
				if err == nil {
					t.Errorf("ValidateUpdate() expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("ValidateUpdate() unexpected error = %v", err)
				}
			}

			// Warnings should always be nil in our implementation
			if warnings != nil {
				t.Errorf("ValidateUpdate() warnings = %v, want nil", warnings)
			}
		})
	}
}

func TestCdk8sAppProxy_ValidateDelete(t *testing.T) {
	proxy := &Cdk8sAppProxy{
		Spec: Cdk8sAppProxySpec{
			GitRepository: &GitRepositorySpec{
				URL: "https://github.com/example/repo",
			},
		},
	}

	ctx := context.Background()
	warnings, err := proxy.ValidateDelete(ctx, proxy)

	if err != nil {
		t.Errorf("ValidateDelete() unexpected error = %v", err)
	}

	if warnings != nil {
		t.Errorf("ValidateDelete() warnings = %v, want nil", warnings)
	}
}

func TestCdk8sAppProxy_validateCdk8sAppProxy(t *testing.T) {
	tests := []struct {
		name         string
		proxy        *Cdk8sAppProxy
		wantWarnings admission.Warnings
		wantError    bool
		errorCount   int
	}{
		{
			name: "multiple validation errors",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					LocalPath: "/local/path",
					GitRepository: &GitRepositorySpec{
						// Missing URL
						Reference: "main",
					},
				},
			},
			wantWarnings: nil,
			wantError:    true,
			errorCount:   2, // Both localPath+gitRepository and missing URL errors
		},
		{
			name: "valid configuration returns no errors",
			proxy: &Cdk8sAppProxy{
				Spec: Cdk8sAppProxySpec{
					GitRepository: &GitRepositorySpec{
						URL:       "https://github.com/example/repo",
						Reference: "main",
						Path:      ".",
					},
				},
			},
			wantWarnings: nil,
			wantError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := tt.proxy.validateCdk8sAppProxy()

			if (err != nil) != tt.wantError {
				t.Errorf("validateCdk8sAppProxy() error = %v, wantError %v", err, tt.wantError)
				return
			}

			// Compare warnings properly
			if tt.wantWarnings == nil && warnings != nil {
				t.Errorf("validateCdk8sAppProxy() warnings = %v, want nil", warnings)
			} else if tt.wantWarnings != nil && warnings == nil {
				t.Errorf("validateCdk8sAppProxy() warnings = nil, want %v", tt.wantWarnings)
			} else if tt.wantWarnings != nil && warnings != nil {
				if len(warnings) != len(tt.wantWarnings) {
					t.Errorf("validateCdk8sAppProxy() warnings length = %d, want %d", len(warnings), len(tt.wantWarnings))
				} else {
					for i, warning := range warnings {
						if warning != tt.wantWarnings[i] {
							t.Errorf("validateCdk8sAppProxy() warning[%d] = %v, want %v", i, warning, tt.wantWarnings[i])
						}
					}
				}
			}

			if tt.wantError && tt.errorCount > 0 {
				// This is a simplified check - in a real scenario you might want to parse the error message
				// to count the actual number of validation errors
				if err == nil {
					t.Errorf("validateCdk8sAppProxy() expected %d errors, got nil", tt.errorCount)
				}
			}
		})
	}
}
