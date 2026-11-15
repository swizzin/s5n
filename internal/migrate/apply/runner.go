package apply

// runner.go is rewrite.Runner for the one format only an app's own binary may
// write: Plex's library database, which stock SQLite cannot touch (its
// full-text tables carry tokenizers compiled into Plex's libsqlite3, see
// internal/migrate/plex).
//
// The runner is a one-shot Job in the seedbox's namespace, running the pinned
// image of the app, with the *staged* copy of the config mounted at /config --
// never the source, and never the release's own claim directory. The Job talks
// to box through files on the config claim, which is a host directory box can
// see: box writes the script's stdin before the Job starts and reads its
// stdout, stderr and exit status after it ends. Nothing has to be streamed into
// a pod, and nothing about the pod is trusted.
//
// The pod is what Pod Security "restricted" admits and what the blessed-image
// policy admits, because it is a pod in a seedbox namespace like every other:
// the release's uid, no privilege escalation, every capability dropped, a
// read-only root filesystem, and an image from the chart's variants.yaml.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/swizzin/s5n/internal/wait"
)

// The runner's two directories inside the plan's staging tree, as subPaths of
// the config claim: the app's staged config, and the files box and the Job pass
// each other.
const (
	runnerDir = ".runner"
	// The one volume's two mounts: the staged config at /config, which is
	// where the chart mounts an app's claim, and the exchange directory at
	// /runner.
	volumeConfig  = "config"
	volumeScratch = "tmp"
	mountConfig   = "/config"
	mountRunner   = "/runner"
	mountScratch  = "/tmp"
	// runnerTimeout bounds one tool run, and is the Job's own deadline so the
	// cluster kills a hung container before box stops waiting on it.
	runnerTimeout = 10 * time.Minute
	// runnerPoll is how often the Job's status is read.
	runnerPoll = 250 * time.Millisecond
)

// RunnerConfig is what one JobRunner needs, all of it decided by the caller:
// the release's namespace and config claim, the plan the staging tree belongs
// to, the image the values select, and the uid every file on a seedbox belongs
// to.
type RunnerConfig struct {
	Client     kubernetes.Interface
	Namespace  string
	Claim      string
	ConfigRoot string
	PlanID     string
	Image      string
	PUID, PGID int
	// Poll and Timeout default to runnerPoll and runnerTimeout; tests shrink
	// them.
	Poll    time.Duration
	Timeout time.Duration
}

// JobRunner runs tools through one-shot Jobs. It is safe for the two passes of
// one rewriter to call it one after the other, and numbers each call so two
// runs' files never meet.
type JobRunner struct {
	cfg RunnerConfig
	mu  sync.Mutex
	n   int
}

// NewRunner builds the runner cmd/box hands apply for the Plex rewriter.
func NewRunner(cfg RunnerConfig) *JobRunner { return &JobRunner{cfg: cfg} }

// Run implements rewrite.Runner: argv[0] is a command inside the image, argv[1:]
// its arguments, and the staged config is at /config. It returns the command's
// stdout; a non-zero exit is an error carrying what it wrote to stderr, because
// that text is the only account of why the app's own tool refused.
func (r *JobRunner) Run(ctx context.Context, app string, argv []string, stdin []byte) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: no command to run", app)
	}
	if r.cfg.Client == nil || r.cfg.Claim == "" {
		return nil, fmt.Errorf("%s: no cluster client or config claim, so its tool cannot run", app)
	}
	r.mu.Lock()
	r.n++
	n := r.n
	r.mu.Unlock()

	name := fmt.Sprintf("%s-%d", dnsLabel(app), n)
	dir := filepath.Join(r.cfg.ConfigRoot, ".box", "migrate", r.cfg.PlanID, runnerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".in"), stdin, 0o600); err != nil {
		return nil, fmt.Errorf("write %s's input: %w", app, err)
	}

	job, err := r.start(ctx, app, name, argv)
	if err != nil {
		r.remove(dir, name)
		return nil, err
	}
	defer r.delete(context.WithoutCancel(ctx), job)
	return r.collect(ctx, app, name, argv[0], dir, job)
}

// start creates the Job, and the caller deletes it whatever happens next.
func (r *JobRunner) start(ctx context.Context, app, name string, argv []string) (*batchv1.Job, error) {
	job := r.job(app, name, argv)
	created, err := r.cfg.Client.BatchV1().Jobs(r.cfg.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("%s: start the job that runs %s: %w", app, argv[0], err)
	}
	return created, nil
}

// collect waits for the Job, reads what it wrote and turns a non-zero status
// into an error.
func (r *JobRunner) collect(ctx context.Context, app, name, command, dir string, job *batchv1.Job) ([]byte, error) {
	finished := false
	waited, err := wait.Until(ctx, r.timeout(), r.poll(), func(ctx context.Context) (bool, error) {
		got, err := r.cfg.Client.BatchV1().Jobs(r.cfg.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Errorf("%s: the job that runs %s was deleted while it ran", app, command)
			}
			return false, err
		}
		finished = got.Status.Succeeded > 0 || got.Status.Failed > 0 || jobFinished(&got.Status)
		return finished, nil
	})
	if err != nil {
		r.remove(dir, name)
		return nil, err
	}
	if !waited.Done || !finished {
		r.remove(dir, name)
		return nil, fmt.Errorf("%s: the job that runs %s did not finish within %s", app, command, r.timeout())
	}

	stdout, stderr, code, readErr := r.read(dir, name)
	r.remove(dir, name)
	if readErr != nil {
		return nil, fmt.Errorf("%s: the job that runs %s finished without leaving its result: %w", app, command, readErr)
	}
	if code != 0 {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = "it wrote nothing to stderr"
		}
		return nil, fmt.Errorf("%s: %s exited %d: %s", app, command, code, detail)
	}
	return stdout, nil
}

// read returns the three files the Job left, and the exit status.
func (r *JobRunner) read(dir, name string) (stdout, stderr []byte, code int, err error) {
	var rc []byte
	if rc, err = os.ReadFile(filepath.Join(dir, name+".rc")); err != nil {
		return nil, nil, 0, err
	}
	n, perr := strconv.Atoi(strings.TrimSpace(string(rc)))
	if perr != nil {
		return nil, nil, 0, fmt.Errorf("%s.rc holds %q, which is not an exit status: %w", name, strings.TrimSpace(string(rc)), perr)
	}
	if stdout, err = os.ReadFile(filepath.Join(dir, name+".out")); err != nil {
		return nil, nil, 0, err
	}
	if stderr, err = os.ReadFile(filepath.Join(dir, name+".err")); err != nil {
		return nil, nil, 0, err
	}
	return stdout, stderr, n, nil
}

// job checks: a Job that carries a Complete or Failed condition is over even
// before the controller has counted it.
func jobFinished(status *batchv1.JobStatus) bool {
	for _, c := range status.Conditions {
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// job is the one-shot Job: the app's own image, the staged config at /config,
// the exchange directory at /runner, and nothing else.
func (r *JobRunner) job(app, name string, argv []string) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "box",
		"app.kubernetes.io/part-of":    "s5n",
		"s5n.io/migrate":               dnsLabel(app),
	}
	script := fmt.Sprintf(`"$0" "$@" </runner/%s.in >/runner/%s.out 2>/runner/%s.err; echo $? >/runner/%s.rc`, name, name, name, name)
	claim := &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.cfg.Claim}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "s5n-migrate-",
			Namespace:    r.cfg.Namespace,
			Labels:       labels,
		},
		Spec: batchv1.JobSpec{
			// One attempt: the tool either answers or it does not, and a
			// retry against a half-written database is worse than a refusal.
			BackoffLimit: ptr.To[int32](0),
			// The cluster's deadline is the fixed one, whatever this run's own
			// budget is: box waiting less than the pod may run only means box
			// deletes it early, and a shortened deadline would tell the
			// cluster to kill a long Plex query that is still working.
			ActiveDeadlineSeconds: ptr.To[int64](int64(runnerTimeout.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					EnableServiceLinks:           ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:           ptr.To[int64](int64(r.cfg.PUID)),
						RunAsGroup:          ptr.To[int64](int64(r.cfg.PGID)),
						FSGroup:             ptr.To[int64](int64(r.cfg.PGID)),
						FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
						RunAsNonRoot:        ptr.To(true),
						SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "tool",
						Image:   r.cfg.Image,
						Command: []string{"sh", "-c", script},
						Args:    argv,
						Env:     []corev1.EnvVar{{Name: "TMPDIR", Value: "/tmp"}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							RunAsNonRoot:             ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: volumeConfig, MountPath: mountConfig, SubPath: filepath.Join(".box", "migrate", r.cfg.PlanID, app)},
							{Name: volumeConfig, MountPath: mountRunner, SubPath: filepath.Join(".box", "migrate", r.cfg.PlanID, runnerDir)},
							{Name: volumeScratch, MountPath: mountScratch},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: volumeConfig, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: claim}},
						{Name: volumeScratch, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// delete removes the Job it created, and everything a Job cascades to. A
// foreground or background delete needs the Job gone before a re-run can reuse
// its files, so the call is not cancellable: a cancelled run still cleans up.
func (r *JobRunner) delete(ctx context.Context, job *batchv1.Job) {
	background := metav1.DeletePropagationBackground
	err := r.cfg.Client.BatchV1().Jobs(r.cfg.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &background})
	if err != nil && !apierrors.IsNotFound(err) {
		// Nothing to do about it here: the Job is labelled and its pod is
		// harmless. A failed run's error is what the operator needs.
		return
	}
}

// remove clears the four files one run made, so a later run's numbering is the
// only thing between two calls.
func (r *JobRunner) remove(dir, name string) {
	for _, ext := range []string{".in", ".out", ".err", ".rc"} {
		os.Remove(filepath.Join(dir, name+ext))
	}
}

func (r *JobRunner) timeout() time.Duration {
	if r.cfg.Timeout > 0 {
		return r.cfg.Timeout
	}
	return runnerTimeout
}

func (r *JobRunner) poll() time.Duration {
	if r.cfg.Poll > 0 {
		return r.cfg.Poll
	}
	return runnerPoll
}

// dnsLabel lowercases name and keeps only what a label may hold, so an instance
// name is always a legal file and label stem.
func dnsLabel(name string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "migrate"
	}
	return out
}
