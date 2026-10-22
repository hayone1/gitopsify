package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hayone1/gitopsify/pkg/kube"
)

func TestWaitReadyExplainsTimeouts(t *testing.T) {
	one := int32(1)
	kc := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "kustomize-controller", Namespace: "flux-system"}, Spec: appsv1.DeploymentSpec{Replicas: &one}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "kustomize-controller-abc", Namespace: "flux-system"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "manager",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "toomanyrequests"}},
			}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "source-controller-def", Namespace: "flux-system"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "manager",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "helm-controller-xyz", Namespace: "flux-system"},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/1 nodes are available: 1 Insufficient memory."}}},
		},
	)
	err := waitReady(context.Background(), &kube.Client{Kubernetes: kc}, "flux-system", []string{"kustomize-controller"}, nil, time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	for _, want := range []string{"deployment/kustomize-controller", "ImagePullBackOff: toomanyrequests", "Insufficient memory", "pulling its image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestMergeKnownHosts(t *testing.T) {
	got := MergeKnownHosts("github.com ssh-ed25519 AAA\n", "# comment\ngit.acme.io ssh-ed25519 BBB\ngithub.com ssh-ed25519 AAA\n")
	if got != "github.com ssh-ed25519 AAA\ngit.acme.io ssh-ed25519 BBB\n" {
		t.Errorf("got %q", got)
	}
}
