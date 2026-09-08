// Package k8s discovers inference workloads from the Kubernetes API.
//
// It uses shared informers rather than a polling loop. Informers hold a watch
// and keep a local cache, so discovery costs one initial List per resource and
// then only deltas — a polling loop re-Lists every pod in the namespace on
// every tick, which is load the API server does not need and which scales with
// the size of the namespace rather than with the rate of change. The cache also
// removes the need for a store of our own: the lister is the store, and objects
// deleted from the cluster leave it automatically, so a workload that is
// deleted stops being reported instead of lingering forever.
//
// Access is read-only. The client is only ever used for List and Watch, and the
// ClusterRole shipped with the chart grants nothing else.
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v2"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Well-known labels and annotations used by IFA's Kubernetes discovery
// convention.
//
// Runtime is a LABEL because it takes values from a small, fixed set ("vllm",
// "triton", …) and is useful for label selectors and field queries.
//
// Model identity is an ANNOTATION because real Hugging Face model IDs contain
// "/" (e.g. "facebook/opt-125m", "meta-llama/Llama-3.1-8B-Instruct"), which
// is not a valid Kubernetes label value character. Annotations do not restrict
// the character set, so they carry the ID without sanitisation. Neither
// convention is required for IFA to function; omitting them leaves the
// corresponding fields empty rather than causing an error.
const (
	LabelRuntime    = "inference.io/runtime"
	AnnotationModel = "inference.io/model"
)

// Workload is the discovered state of one inference deployment.
type Workload struct {
	Name          string
	Namespace     string
	Runtime       string
	ModelName     string
	Replicas      int32
	ReadyReplicas int32
	// MaxReplicas comes from a HorizontalPodAutoscaler targeting this
	// deployment, and is zero when none exists. Without it the engine cannot
	// tell "queueing and the autoscaler will fix it" from "queueing at the
	// ceiling and nothing will".
	MaxReplicas  int32
	RestartCount int32
	GPURequest   string
	Labels       map[string]string
	LastUpdated  time.Time
}

// Options configures the watcher.
type Options struct {
	// Namespace to watch. Empty means all namespaces, which requires a
	// ClusterRole rather than a namespaced Role.
	Namespace string
	// LabelSelector restricts discovery. Empty watches every deployment in
	// scope, which on a busy namespace reports a great deal that has nothing to
	// do with inference.
	LabelSelector string
	// ResyncPeriod is the informer's full-resync interval. Informers stay
	// current from the watch stream; resync is a safety net against a missed
	// event, so it is measured in minutes, not seconds.
	ResyncPeriod time.Duration
	Logger       *slog.Logger
}

const defaultResync = 10 * time.Minute

// Watcher holds the informer caches.
type Watcher struct {
	deployments appslisters.DeploymentLister
	pods        corelisters.PodLister
	hpas        autoscalinglisters.HorizontalPodAutoscalerLister
	factory     informers.SharedInformerFactory
	selector    labels.Selector
	namespace   string
	log         *slog.Logger
}

// NewWatcher builds a watcher. It uses in-cluster credentials when running in a
// pod and falls back to the local kubeconfig otherwise, so the same binary runs
// in both places.
func NewWatcher(opts Options) (*Watcher, error) {
	if opts.Logger == nil {
		return nil, fmt.Errorf("k8s: logger is required")
	}
	if opts.ResyncPeriod <= 0 {
		opts.ResyncPeriod = defaultResync
	}

	selector := labels.Everything()
	if opts.LabelSelector != "" {
		parsed, err := labels.Parse(opts.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("k8s: parsing label selector %q: %w", opts.LabelSelector, err)
		}
		selector = parsed
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("k8s: no in-cluster config and no usable kubeconfig: %w", err)
		}
	}
	// Discovery is not latency-critical and should never be the reason the API
	// server is under pressure.
	cfg.QPS = 10
	cfg.Burst = 20
	cfg.UserAgent = "ifa"

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: building client: %w", err)
	}
	return newWatcher(client, opts, selector)
}

// newWatcher builds a watcher around an existing client. Splitting it out lets
// tests drive the informers with a fake clientset instead of a cluster.
func newWatcher(client kubernetes.Interface, opts Options, selector labels.Selector) (*Watcher, error) {
	factoryOpts := []informers.SharedInformerOption{
		informers.WithNamespace(opts.Namespace),
	}
	if opts.LabelSelector != "" {
		factoryOpts = append(factoryOpts, informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			lo.LabelSelector = opts.LabelSelector
		}))
	}
	factory := informers.NewSharedInformerFactoryWithOptions(client, opts.ResyncPeriod, factoryOpts...)

	return &Watcher{
		deployments: factory.Apps().V1().Deployments().Lister(),
		pods:        factory.Core().V1().Pods().Lister(),
		hpas:        factory.Autoscaling().V2().HorizontalPodAutoscalers().Lister(),
		factory:     factory,
		selector:    selector,
		namespace:   opts.Namespace,
		log:         opts.Logger,
	}, nil
}

// Start begins the informers and blocks until their caches have synced or ctx
// is cancelled.
//
// Blocking on the initial sync matters: serving an empty workload list while
// the cache fills would make every scaling rule silently skip, and an operator
// would read that as "no scaling problems".
func (w *Watcher) Start(ctx context.Context) error {
	w.factory.Start(ctx.Done())

	synced := w.factory.WaitForCacheSync(ctx.Done())
	for typ, ok := range synced {
		if !ok {
			return fmt.Errorf("k8s: informer cache for %s did not sync", typ)
		}
	}
	w.log.Info("kubernetes discovery ready",
		"namespace", namespaceLabel(w.namespace), "selector", w.selector.String())
	return nil
}

func namespaceLabel(ns string) string {
	if ns == "" {
		return "(all)"
	}
	return ns
}

// All returns every discovered workload.
func (w *Watcher) All() []Workload {
	deployments, err := w.deployments.List(w.selector)
	if err != nil {
		w.log.Warn("listing deployments from cache failed", "err", err)
		return nil
	}

	out := make([]Workload, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, w.workloadFor(d))
	}
	return out
}

// Replicas implements the collector's WorkloadLookup. It returns false for a
// workload the watcher has not discovered, so the scaling rules stay dormant
// rather than treating an unknown deployment as one with zero replicas.
func (w *Watcher) Replicas(namespace, name string) (desired, ready, max int32, ok bool) {
	d, err := w.deployments.Deployments(namespace).Get(name)
	if err != nil {
		return 0, 0, 0, false
	}
	wl := w.workloadFor(d)
	return wl.Replicas, wl.ReadyReplicas, wl.MaxReplicas, true
}

func (w *Watcher) workloadFor(d *appsv1.Deployment) Workload {
	wl := Workload{
		Name:          d.Name,
		Namespace:     d.Namespace,
		Runtime:       d.Labels[LabelRuntime],
		ModelName:     d.Annotations[AnnotationModel],
		ReadyReplicas: d.Status.ReadyReplicas,
		Labels:        d.Labels,
		LastUpdated:   time.Now().UTC(),
	}
	// Spec.Replicas is a pointer because "unset" means "defaulted to 1"; the
	// status field is what the cluster actually wants right now.
	wl.Replicas = d.Status.Replicas
	if d.Spec.Replicas != nil {
		wl.Replicas = *d.Spec.Replicas
	}
	wl.GPURequest = gpuRequest(d.Spec.Template.Spec.Containers)
	wl.RestartCount = w.restartsFor(d)
	wl.MaxReplicas = w.maxReplicasFor(d)
	return wl
}

// restartsFor sums container restarts across the deployment's pods. Restarts
// are a pod-level fact, and a rising count alongside queueing is what separates
// "slow to start" from "crash-looping".
func (w *Watcher) restartsFor(d *appsv1.Deployment) int32 {
	if d.Spec.Selector == nil {
		return 0
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return 0
	}
	pods, err := w.pods.Pods(d.Namespace).List(sel)
	if err != nil {
		return 0
	}
	var total int32
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			total += cs.RestartCount
		}
	}
	return total
}

// maxReplicasFor finds an HPA targeting this deployment.
func (w *Watcher) maxReplicasFor(d *appsv1.Deployment) int32 {
	hpas, err := w.hpas.HorizontalPodAutoscalers(d.Namespace).List(labels.Everything())
	if err != nil {
		return 0
	}
	for _, h := range hpas {
		ref := h.Spec.ScaleTargetRef
		if ref.Kind == "Deployment" && ref.Name == d.Name {
			return h.Spec.MaxReplicas
		}
	}
	return 0
}

// gpuRequest returns the first nvidia.com/gpu request found across containers.
func gpuRequest(containers []corev1.Container) string {
	for _, c := range containers {
		if q, ok := c.Resources.Requests["nvidia.com/gpu"]; ok {
			return q.String()
		}
		if q, ok := c.Resources.Limits["nvidia.com/gpu"]; ok {
			return q.String()
		}
	}
	return ""
}

// compile-time assertion that the HPA lister import is used for the v2 API.
var _ = autoscalingv2.HorizontalPodAutoscaler{}
