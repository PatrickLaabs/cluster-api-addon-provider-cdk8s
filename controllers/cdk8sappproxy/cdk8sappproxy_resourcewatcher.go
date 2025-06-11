package cdk8sappproxy

import (
	"context"
	"sync"
	"time"

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ResourceWatcher watches resources on target clusters.
type ResourceWatcher interface {
	Watch(ctx context.Context, config WatchConfig) error
}

// EventHandler handles different types of watch events.
type EventHandler interface {
	OnDeleted(ctx context.Context, config WatchConfig) error
	OnOther(ctx context.Context, eventType watch.EventType, config WatchConfig) error
	OnClosed(ctx context.Context, config WatchConfig)
	OnError(ctx context.Context, err error, config WatchConfig)

	SetWatchManager(wm WatchManager)
}

// WatchManager manages the lifecycle of resource watches.
type WatchManager interface {
	Start(ctx context.Context, config WatchConfig) error
	Stop(parentProxy types.NamespacedName, watchKey string)
	Cleanup(parentProxy types.NamespacedName)
}

// WatchConfig encapsulates all watch configuration.
type WatchConfig struct {
	TargetClient dynamic.Interface
	GVK          schema.GroupVersionKind
	Namespace    string
	Name         string
	ParentProxy  types.NamespacedName
	WatchKey     string
}

// resourceWatcher implements ResourceWatcher.
type resourceWatcher struct {
	events EventHandler
}

// NewResourceWatcher creates a new resource watcher.
func NewResourceWatcher(events EventHandler) ResourceWatcher {
	return &resourceWatcher{
		events: events,
	}
}

func (w *resourceWatcher) Watch(ctx context.Context, config WatchConfig) error {
	logger := log.FromContext(ctx).WithValues(
		"watchKey", config.WatchKey,
		"gvk", config.GVK.String(),
		"resourceNamespace", config.Namespace,
		"resourceName", config.Name,
		"parentProxy", config.ParentProxy.String(),
	)
	logger.Info("Starting resource watch")

	watcher, err := w.createWatcher(ctx, config)
	if err != nil {
		w.events.OnError(ctx, err, config)

		return err
	}
	defer watcher.Stop()

	return w.processEvents(ctx, watcher, config, logger)
}

func (w *resourceWatcher) createWatcher(ctx context.Context, config WatchConfig) (watch.Interface, error) {
	gvr := config.GVK.GroupVersion().WithResource(getPluralFromKind(config.GVK.Kind))

	return config.TargetClient.Resource(gvr).Namespace(config.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + config.Name,
	})
}

func (w *resourceWatcher) processEvents(ctx context.Context, watcher watch.Interface, config WatchConfig, logger logr.Logger) error {
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				w.events.OnClosed(ctx, config)

				return nil
			}

			logger.Info("Received watch event", "type", event.Type)

			if event.Type == watch.Deleted {
				if err := w.events.OnDeleted(ctx, config); err != nil {
					logger.Error(err, "Failed to handle deletion")
				}

				return nil
			}

			if err := w.events.OnOther(ctx, event.Type, config); err != nil {
				logger.Error(err, "Failed to handle event", "type", event.Type)
			}

		case <-ctx.Done():
			logger.Info("Watch cancelled")

			return ctx.Err()
		}
	}
}

// eventHandler implements EventHandler.
type eventHandler struct {
	client       client.Client
	watchManager WatchManager
}

// NewEventHandler creates a new event handler.
func NewEventHandler(client client.Client, watchManager WatchManager) EventHandler {
	return &eventHandler{
		client:       client,
		watchManager: watchManager,
	}
}

// Add a setter method for the circular dependency.
func (h *eventHandler) SetWatchManager(wm WatchManager) {
	h.watchManager = wm
}

func (h *eventHandler) OnDeleted(ctx context.Context, config WatchConfig) error {
	logger := log.FromContext(ctx).WithValues("parentProxy", config.ParentProxy.String())
	logger.Info("Resource deleted, triggering parent reconciliation")

	if err := h.triggerReconciliation(ctx, config.ParentProxy); err != nil {
		logger.Error(err, "Failed to trigger reconciliation")

		return err
	}

	// Only call Stop if watchManager is set
	if h.watchManager != nil {
		h.watchManager.Stop(config.ParentProxy, config.WatchKey)
	}

	return nil
}

// ToDo.
func (h *eventHandler) OnOther(ctx context.Context, eventType watch.EventType, config WatchConfig) error {
	logger := log.FromContext(ctx)
	logger.Info("Received non-deletion event", "type", eventType)
	// Continue watching - reconciliation loop handles desired state
	return nil
}

func (h *eventHandler) OnClosed(ctx context.Context, config WatchConfig) {
	logger := log.FromContext(ctx)
	logger.Info("Watch closed")

	if h.watchManager != nil {
		h.watchManager.Stop(config.ParentProxy, config.WatchKey)
	}
}

func (h *eventHandler) OnError(ctx context.Context, err error, config WatchConfig) {
	logger := log.FromContext(ctx)
	logger.Error(err, "Watch failed")

	if h.watchManager != nil {
		h.watchManager.Stop(config.ParentProxy, config.WatchKey)
	}
}

func (h *eventHandler) triggerReconciliation(ctx context.Context, parentProxy types.NamespacedName) error {
	proxy := &addonsv1alpha1.Cdk8sAppProxy{}
	if err := h.client.Get(ctx, parentProxy, proxy); err != nil {
		return err
	}

	if proxy.Annotations == nil {
		proxy.Annotations = make(map[string]string)
	}

	proxy.Annotations["cdk8s.addons.cluster.x-k8s.io/reconcile-trigger"] = metav1.Now().Format(time.RFC3339Nano)

	return h.client.Update(ctx, proxy)
}

// watchManager implements WatchManager.
type watchManager struct {
	mu              sync.RWMutex
	watches         map[types.NamespacedName]map[string]context.CancelFunc
	resourceWatcher ResourceWatcher
}

// NewWatchManager creates a new watch manager.
func NewWatchManager(resourceWatcher ResourceWatcher) WatchManager {
	return &watchManager{
		watches:         make(map[types.NamespacedName]map[string]context.CancelFunc),
		resourceWatcher: resourceWatcher,
	}
}

func (m *watchManager) Start(ctx context.Context, config WatchConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger := log.FromContext(ctx)

	if m.isActive(config.ParentProxy, config.WatchKey) {
		logger.Info("Watch already active", "key", config.WatchKey)

		return nil
	}

	watchCtx, cancel := context.WithCancel(ctx)
	m.store(config.ParentProxy, config.WatchKey, cancel)

	go func() {
		defer cancel()
		if err := m.resourceWatcher.Watch(watchCtx, config); err != nil {
			logger.Error(err, "Watch failed", "key", config.WatchKey)
		}
	}()

	return nil
}

func (m *watchManager) Stop(parentProxy types.NamespacedName, watchKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.watches[parentProxy] != nil {
		if cancel, ok := m.watches[parentProxy][watchKey]; ok {
			cancel()
			delete(m.watches[parentProxy], watchKey)
		}
	}
}

func (m *watchManager) Cleanup(parentProxy types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if watches, ok := m.watches[parentProxy]; ok {
		for _, cancel := range watches {
			cancel()
		}
		delete(m.watches, parentProxy)
	}
}

func (m *watchManager) isActive(parentProxy types.NamespacedName, watchKey string) bool {
	if m.watches[parentProxy] == nil {
		return false
	}
	_, exists := m.watches[parentProxy][watchKey]

	return exists
}

func (m *watchManager) store(parentProxy types.NamespacedName, watchKey string, cancel context.CancelFunc) {
	if m.watches[parentProxy] == nil {
		m.watches[parentProxy] = make(map[string]context.CancelFunc)
	}
	m.watches[parentProxy][watchKey] = cancel
}

func (r *Reconciler) startResourceWatch(ctx context.Context, targetClient dynamic.Interface, gvk schema.GroupVersionKind, namespace, name string, parentProxy types.NamespacedName, watchKey string) error {
	config := WatchConfig{
		TargetClient: targetClient,
		GVK:          gvk,
		Namespace:    namespace,
		Name:         name,
		ParentProxy:  parentProxy,
		WatchKey:     watchKey,
	}

	return r.WatchManager.Start(ctx, config)
}
