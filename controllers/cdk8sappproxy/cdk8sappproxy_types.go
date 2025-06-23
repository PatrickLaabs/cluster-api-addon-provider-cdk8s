package cdk8sappproxy

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reconciler reconciles a Cdk8sAppProxy object.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	WatchManager     *ResourceWatchManager
	ActiveGitPollers map[types.NamespacedName]context.CancelFunc
}
