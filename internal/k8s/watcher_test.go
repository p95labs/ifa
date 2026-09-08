package k8s

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func deployment(ns, name string, replicas, ready int32, lbls, anns map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbls, Annotations: anns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "server",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("2"),
						},
					},
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: ready},
	}
}

func pod(ns, name, app string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": app}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "server", RestartCount: restarts}},
		},
	}
}

func hpa(ns, name, target string, max int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MaxReplicas:    max,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: target},
		},
	}
}

func startWatcher(t *testing.T, opts Options, objects ...runtime.Object) *Watcher {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discard()
	}
	if opts.ResyncPeriod == 0 {
		opts.ResyncPeriod = time.Hour
	}

	selector := labels.Everything()
	if opts.LabelSelector != "" {
		parsed, err := labels.Parse(opts.LabelSelector)
		if err != nil {
			t.Fatalf("parsing selector: %v", err)
		}
		selector = parsed
	}

	w, err := newWatcher(fake.NewSimpleClientset(objects...), opts, selector)
	if err != nil {
		t.Fatalf("newWatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return w
}

func TestDiscoversDeploymentsWithReplicaCounts(t *testing.T) {
	w := startWatcher(t, Options{Namespace: "inference"},
		deployment("inference", "chat", 4, 2,
			map[string]string{LabelRuntime: "vllm"},
			map[string]string{AnnotationModel: "meta-llama/Llama-3.1-8B-Instruct"},
		),
		pod("inference", "chat-a", "chat", 3),
		pod("inference", "chat-b", "chat", 1),
		hpa("inference", "chat-hpa", "chat", 12),
	)

	all := w.All()
	if len(all) != 1 {
		t.Fatalf("discovered %d workloads, want 1", len(all))
	}
	got := all[0]

	if got.Name != "chat" || got.Namespace != "inference" {
		t.Errorf("identity = %s/%s", got.Namespace, got.Name)
	}
	if got.Runtime != "vllm" || got.ModelName != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Errorf("metadata not read: runtime=%q model=%q", got.Runtime, got.ModelName)
	}
	if got.Replicas != 4 || got.ReadyReplicas != 2 {
		t.Errorf("replicas = %d/%d, want 4/2", got.Replicas, got.ReadyReplicas)
	}
	// Restarts are summed across the deployment's pods: a rising count next to
	// a queue is what separates "slow to start" from "crash-looping".
	if got.RestartCount != 4 {
		t.Errorf("restart count = %d, want 4 (3+1 across pods)", got.RestartCount)
	}
	if got.GPURequest != "2" {
		t.Errorf("GPU request = %q, want 2", got.GPURequest)
	}
	if got.MaxReplicas != 12 {
		t.Errorf("max replicas = %d, want 12 from the HPA", got.MaxReplicas)
	}
}

// Without an HPA there is no ceiling, and IFA-SCL-001 must stay dormant rather
// than inventing one.
func TestNoHPAMeansNoCeiling(t *testing.T) {
	w := startWatcher(t, Options{Namespace: "inference"},
		deployment("inference", "chat", 3, 3, nil, nil))

	_, _, max, ok := w.Replicas("inference", "chat")
	if !ok {
		t.Fatal("deployment not found")
	}
	if max != 0 {
		t.Errorf("max replicas = %d, want 0 when no HPA targets the deployment", max)
	}
}

// An HPA pointing at something else must not be attributed to this deployment.
func TestHPAForAnotherDeploymentIsIgnored(t *testing.T) {
	w := startWatcher(t, Options{Namespace: "inference"},
		deployment("inference", "chat", 3, 3, nil, nil),
		hpa("inference", "other-hpa", "some-other-deployment", 40),
	)
	_, _, max, _ := w.Replicas("inference", "chat")
	if max != 0 {
		t.Errorf("max replicas = %d; an unrelated HPA was attributed to this deployment", max)
	}
}

// Returning false rather than zeroes is what keeps the scaling rules from
// treating an unknown deployment as one with no replicas — which would look
// exactly like an outage.
func TestUnknownWorkloadReportsNotFound(t *testing.T) {
	w := startWatcher(t, Options{Namespace: "inference"},
		deployment("inference", "chat", 3, 3, nil, nil))

	if _, _, _, ok := w.Replicas("inference", "does-not-exist"); ok {
		t.Error("an unknown workload was reported as found")
	}
	if _, _, _, ok := w.Replicas("other-namespace", "chat"); ok {
		t.Error("a workload in another namespace was reported as found")
	}
}

func TestLabelSelectorNarrowsDiscovery(t *testing.T) {
	objects := []runtime.Object{
		deployment("inference", "chat", 1, 1, map[string]string{LabelRuntime: "vllm"}, nil),
		deployment("inference", "unrelated-web-app", 1, 1, map[string]string{"app": "web"}, nil),
	}

	all := startWatcher(t, Options{Namespace: "inference"}, objects...).All()
	if len(all) != 2 {
		t.Fatalf("without a selector, got %d workloads, want 2", len(all))
	}

	filtered := startWatcher(t, Options{Namespace: "inference", LabelSelector: LabelRuntime}, objects...).All()
	if len(filtered) != 1 || filtered[0].Name != "chat" {
		t.Errorf("selector did not narrow discovery: %v", filtered)
	}
}

func TestPodsOfAnotherDeploymentDoNotCountAsRestarts(t *testing.T) {
	w := startWatcher(t, Options{Namespace: "inference"},
		deployment("inference", "chat", 1, 1, nil, nil),
		pod("inference", "chat-a", "chat", 2),
		pod("inference", "other-x", "other", 99),
	)
	all := w.All()
	if all[0].RestartCount != 2 {
		t.Errorf("restart count = %d, want 2 — another deployment's pods were counted", all[0].RestartCount)
	}
}

// TestModelAnnotation is the regression suite for the label-to-annotation
// migration. The real Kubernetes API server rejects Hugging Face model IDs
// (e.g. "facebook/opt-125m") as label values because '/' is not an allowed
// character. The fake clientset used by other tests does not enforce this
// validation, which is exactly why this class of bug goes undetected until a
// real API server is involved. Model identity therefore lives in an annotation
// where the character set is unrestricted.
func TestModelAnnotation(t *testing.T) {
	t.Run("model comes from annotation not label", func(t *testing.T) {
		w := startWatcher(t, Options{Namespace: "inference"},
			deployment("inference", "svc", 1, 1,
				map[string]string{LabelRuntime: "vllm"},
				map[string]string{AnnotationModel: "facebook/opt-125m"},
			),
		)
		got := w.All()[0]
		if got.Runtime != "vllm" {
			t.Errorf("runtime = %q, want \"vllm\" (from label)", got.Runtime)
		}
		if got.ModelName != "facebook/opt-125m" {
			t.Errorf("model = %q, want \"facebook/opt-125m\" (from annotation)", got.ModelName)
		}
	})

	t.Run("slash in model ID is preserved exactly", func(t *testing.T) {
		// Real Kubernetes rejects '/' in label values. This test documents why
		// model names must be annotations, and it fails if someone moves the
		// model back to a label (since the stored string would be sanitised or
		// the API server would reject the manifest).
		const modelID = "meta-llama/Llama-3.1-8B-Instruct"
		w := startWatcher(t, Options{Namespace: "inference"},
			deployment("inference", "svc", 1, 1, nil,
				map[string]string{AnnotationModel: modelID}),
		)
		if got := w.All()[0].ModelName; got != modelID {
			t.Errorf("model = %q, want %q — '/' must survive round-trip", got, modelID)
		}
	})

	t.Run("missing annotation returns empty string without panic", func(t *testing.T) {
		w := startWatcher(t, Options{Namespace: "inference"},
			deployment("inference", "svc", 1, 1,
				map[string]string{LabelRuntime: "vllm"}, nil),
		)
		got := w.All()[0]
		if got.ModelName != "" {
			t.Errorf("model = %q, want \"\" when annotation is absent", got.ModelName)
		}
		if got.Runtime != "vllm" {
			t.Errorf("runtime = %q, want \"vllm\" — label must still be read when annotation is absent", got.Runtime)
		}
	})

	t.Run("model annotation does not appear in label map", func(t *testing.T) {
		// Validates that model identity has been fully removed from labels so
		// it cannot accidentally be used as a selector.
		w := startWatcher(t, Options{Namespace: "inference"},
			deployment("inference", "svc", 1, 1,
				map[string]string{LabelRuntime: "vllm"},
				map[string]string{AnnotationModel: "facebook/opt-125m"},
			),
		)
		if _, ok := w.All()[0].Labels[AnnotationModel]; ok {
			t.Error("inference.io/model found in Labels — it must only appear as an annotation")
		}
	})
}

func TestInvalidLabelSelectorIsRejected(t *testing.T) {
	_, err := NewWatcher(Options{Logger: discard(), LabelSelector: "!!!not a selector"})
	if err == nil {
		t.Fatal("an invalid label selector was accepted")
	}
}

func TestLoggerIsRequired(t *testing.T) {
	if _, err := NewWatcher(Options{}); err == nil {
		t.Error("a watcher without a logger was accepted")
	}
}
