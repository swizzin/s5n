package release

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/kube"
)

// SecretName is the chart's Secret for release `s5n` (secret.yaml): every
// credential, generated or preserved by `lookup`. Box only adds keys the chart
// then keeps.
func SecretName() string { return ReleaseName + "-keys" }

// SecretKeys is the release Secret key by key; the implementation decodes and
// encodes, and nothing prints a value.
type SecretKeys interface {
	// Get returns one key's value, "" when absent.
	Get(ctx context.Context, key string) (string, error)
	// Set writes one data key's value.
	Set(ctx context.Context, key, value string) error
}

// KubeSecret is the release Secret in a live cluster.
type KubeSecret struct {
	namespace string
	name      string
	cs        kubernetes.Interface
}

// NewSecret opens the release Secret of one namespace.
func NewSecret(kubeconfig, namespace string) (*KubeSecret, error) {
	if namespace == "" {
		return nil, fmt.Errorf("no namespace: the grant names the release namespace")
	}
	cfg, err := kube.Config(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("secret client for %s: %w", namespace, err)
	}
	return NewSecretFor(namespace, cs), nil
}

// NewSecretFor is NewSecret over an existing clientset, which is what the tests
// drive.
func NewSecretFor(namespace string, cs kubernetes.Interface) *KubeSecret {
	return &KubeSecret{namespace: namespace, name: SecretName(), cs: cs}
}

// Get reads one key; a missing Secret is an absent key, because apply asks
// before the release that creates it exists.
func (s *KubeSecret) Get(ctx context.Context, key string) (string, error) {
	sec, err := s.secret(ctx)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read secret %s in %s: %w", s.name, s.namespace, err)
	}
	return string(sec.Data[key]), nil
}

// Set writes one key and leaves others; the read first carries a
// resourceVersion, so a concurrent writer is refused rather than overwritten.
func (s *KubeSecret) Set(ctx context.Context, key, value string) error {
	sec, err := s.secret(ctx)
	if err != nil {
		return fmt.Errorf("read secret %s in %s: %w", s.name, s.namespace, err)
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[key] = []byte(value)
	if _, err := s.cs.CoreV1().Secrets(s.namespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("write secret %s in %s: %w", s.name, s.namespace, err)
	}
	return nil
}

func (s *KubeSecret) secret(ctx context.Context) (*corev1.Secret, error) {
	sec, err := s.cs.CoreV1().Secrets(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return sec, nil
}
