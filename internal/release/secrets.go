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

// SecretName is the Secret the chart renders for one release
// (charts/s5n/templates/secret.yaml): every credential the apps and the wiring
// Job read, generated or preserved across upgrades by `lookup`.
//
// It is the chart's name for release `s5n` in its own namespace, and box is the
// only thing that writes into it: the keys the chart generates are rendered
// from an empty Secret, so a key box fills in is one the chart then keeps.
func SecretName() string { return ReleaseName + "-keys" }

// SecretKeys is the release Secret, as one data key at a time.
//
// apply reads and, when Plex has been claimed since the install, writes one key
// of it (plextoken.go). The implementation decodes on the way in and encodes on
// the way out, so a caller only ever handles the value, and nothing here prints
// one.
type SecretKeys interface {
	// Get returns one data key's value, "" when the key is absent.
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

// Get reads one key. The Secret's data is plain base64 of the value, which is
// what corev1.Secret already holds decoded. A Secret that is not there yet is
// an absent key, not a failed read: apply asks an app's key before the release
// that creates it exists.
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

// Set writes one key and leaves every other key as it was. The object is read
// first, so the update carries its resourceVersion: a concurrent writer is
// refused rather than overwritten.
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
