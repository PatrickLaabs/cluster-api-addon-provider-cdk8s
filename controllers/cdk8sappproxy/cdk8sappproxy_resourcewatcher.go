package cdk8sappproxy

import (
	"context"
	"sync"
	"time"

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ResourceWatchManager manages resource watches on target clusters.
type ResourceWatchManager struct {
	mu      sync.RWMutex
	watches map[types.NamespacedName]map[string]context.CancelFunc
	client  client.Client
}

// NewResourceWatchManager creates a new resource watch manager.
func NewResourceWatchManager(client client.Client) *ResourceWatchManager {
	return &ResourceWatchManager{
		watches: make(map[types.NamespacedName]map[string]context.CancelFunc),
		client:  client,
	}
}

// StartWatch starts watching a resource for deletion.
func (m *ResourceWatchManager) StartWatch(ctx context.Context, targetClient dynamic.Interface, gvk schema.GroupVersionKind, namespace, name string, parentProxy types.NamespacedName) error {
	watchKey := gvk.String() + "/" + namespace + "/" + name

	m.mu.Lock()
	defer m.mu.Unlock()

	logger := log.FromContext(ctx).WithValues("watchKey", watchKey, "parentProxy", parentProxy.String())

	// Check if already watching
	if m.isActive(parentProxy, watchKey) {
		logger.Info("Watch already active")

		return nil
	}

	// Start the watch
	watchCtx, cancel := context.WithCancel(ctx)
	m.store(parentProxy, watchKey, cancel)

	go func() {
		defer cancel()
		if err := m.watchResource(watchCtx, targetClient, gvk, namespace, name, parentProxy, watchKey); err != nil {
			logger.Error(err, "Watch failed")
		}
	}()

	logger.Info("Started resource watch")

	return nil
}

// StopWatch stops a specific watch.
func (m *ResourceWatchManager) StopWatch(parentProxy types.NamespacedName, watchKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.watches[parentProxy] != nil {
		if cancel, ok := m.watches[parentProxy][watchKey]; ok {
			cancel()
			delete(m.watches[parentProxy], watchKey)
		}
	}
}

// CleanupWatches stops all watches for a parent proxy.
func (m *ResourceWatchManager) CleanupWatches(parentProxy types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if watches, ok := m.watches[parentProxy]; ok {
		for _, cancel := range watches {
			cancel()
		}
		delete(m.watches, parentProxy)
	}
}

// watchResource performs the actual watching logic.
func (m *ResourceWatchManager) watchResource(ctx context.Context, targetClient dynamic.Interface, gvk schema.GroupVersionKind, namespace, name string, parentProxy types.NamespacedName, watchKey string) error {
	logger := log.FromContext(ctx).WithValues(
		"gvk", gvk.String(),
		"namespace", namespace,
		"name", name,
		"parentProxy", parentProxy.String(),
	)

	gvr := gvk.GroupVersion().WithResource(getPluralFromKind(gvk.Kind))

	watcher, err := targetClient.Resource(gvr).Namespace(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		logger.Error(err, "Failed to create watcher")

		return err
	}
	defer watcher.Stop()

	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				logger.Info("Watch closed")
				m.StopWatch(parentProxy, watchKey)

				return nil
			}

			if event.Type == watch.Deleted {
				logger.Info("Resource deleted, triggering reconciliation")
				if err := m.triggerReconciliation(ctx, parentProxy); err != nil {
					logger.Error(err, "Failed to trigger reconciliation")
				}
				m.StopWatch(parentProxy, watchKey)

				return nil
			}

			// For other events (Added, Modified), just continue watching
			logger.V(1).Info("Received non-deletion event", "type", event.Type)

		case <-ctx.Done():
			logger.Info("Watch cancelled")

			return ctx.Err()
		}
	}
}

// triggerReconciliation triggers a reconciliation by updating the parent proxy.
func (m *ResourceWatchManager) triggerReconciliation(ctx context.Context, parentProxy types.NamespacedName) error {
	proxy := &addonsv1alpha1.Cdk8sAppProxy{}
	if err := m.client.Get(ctx, parentProxy, proxy); err != nil {
		return err
	}

	if proxy.Annotations == nil {
		proxy.Annotations = make(map[string]string)
	}

	proxy.Annotations["cdk8s.addons.cluster.x-k8s.io/reconcile-trigger"] = metav1.Now().Format(time.RFC3339Nano)

	return m.client.Update(ctx, proxy)
}

func (m *ResourceWatchManager) isActive(parentProxy types.NamespacedName, watchKey string) bool {
	if m.watches[parentProxy] == nil {
		return false
	}
	_, exists := m.watches[parentProxy][watchKey]

	return exists
}

func (m *ResourceWatchManager) store(parentProxy types.NamespacedName, watchKey string, cancel context.CancelFunc) {
	if m.watches[parentProxy] == nil {
		m.watches[parentProxy] = make(map[string]context.CancelFunc)
	}
	m.watches[parentProxy][watchKey] = cancel
}
