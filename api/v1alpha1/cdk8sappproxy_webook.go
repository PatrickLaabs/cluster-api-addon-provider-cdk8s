package v1alpha1

import (
	"fmt"

	runtime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var cdk8sappproxylog = logf.Log.WithName("cdk8sappproxy-resource")

func (r *Cdk8sAppProxy) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-addons-cluster-x-k8s-io-v1alpha1-cdk8sappproxy,mutating=true,failurePolicy=fail,sideEffects=None,groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies,verbs=create;update,versions=v1alpha1,name=cdk8sappproxy.kb.io,admissionReviewVersions=v1

// var _ webhook.CustomDefaulter = &Cdk8sAppProxy{}
var _ webhook.Defaulter = &Cdk8sAppProxy{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the type
func (r *Cdk8sAppProxy) Default() {
	cdk8sappproxylog.Info("default", "name", r.Name)

	// Set default git reference if not specified
	if r.Spec.GitRepository != nil && r.Spec.GitRepository.Reference == "" {
		r.Spec.GitRepository.Reference = "main"
	}

	// Set default path if not specified
	if r.Spec.GitRepository != nil && r.Spec.GitRepository.Path == "" {
		r.Spec.GitRepository.Path = "."
	}
}

// +kubebuilder:webhook:path=/validate-addons-cluster-x-k8s-io-v1alpha1-cdk8sappproxy,mutating=false,failurePolicy=fail,sideEffects=None,groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies,verbs=create;update,versions=v1alpha1,name=cdk8sappproxy.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &Cdk8sAppProxy{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type
func (r *Cdk8sAppProxy) ValidateCreate() (admission.Warnings, error) {
	cdk8sappproxylog.Info("validate create", "name", r.Name)

	return r.validateCdk8sAppProxy()
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type
func (r *Cdk8sAppProxy) ValidateUpdate(oldRaw runtime.Object) (admission.Warnings, error) {
	cdk8sappproxylog.Info("validate update", "name", r.Name)

	return r.validateCdk8sAppProxy()
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type
func (r *Cdk8sAppProxy) ValidateDelete() (admission.Warnings, error) {
	cdk8sappproxylog.Info("validate delete", "name", r.Name)

	// No validation needed for delete
	return nil, nil
}

func (r *Cdk8sAppProxy) validateCdk8sAppProxy() (admission.Warnings, error) {
	var allErrs []error

	// Validate that either LocalPath or GitRepository is specified, but not both
	if r.Spec.LocalPath != "" && r.Spec.GitRepository != nil {
		allErrs = append(allErrs, fmt.Errorf("only one of localPath or gitRepository can be specified"))
	}

	if r.Spec.LocalPath == "" && r.Spec.GitRepository == nil {
		allErrs = append(allErrs, fmt.Errorf("either localPath or gitRepository must be specified"))
	}

	// Validate GitRepository fields if specified
	if r.Spec.GitRepository != nil {
		if r.Spec.GitRepository.URL == "" {
			allErrs = append(allErrs, fmt.Errorf("gitRepository.url is required when gitRepository is specified"))
		}
	}

	if len(allErrs) > 0 {
		return nil, fmt.Errorf("validation failed: %v", allErrs)
	}

	return nil, nil
}
