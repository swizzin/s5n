package apply

// runner_test.go drives the Job runner against client-go's fake: the Job's spec,
// the file protocol box and the pod pass each other, and the three ways a run
// ends (success, a non-zero exit, a cancellation).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/swizzin/s5n/internal/admission"
)

// runnerFixture is one runner and the fake cluster it talks to.
type runnerFixture struct {
	t      *testing.T
	cs     *fake.Clientset
	runner *JobRunner
	root   string
	planID string
	// dir is the exchange directory, which the fake pod also writes to.
	dir string
	// job is the Job the fake cluster was asked to create.
	job *batchv1.Job
	// created is closed once the Job exists, so a test can cancel after it.
	created chan struct{}
}

// newRunnerFixture builds a runner with a fake cluster whose "pod" answers the
// file protocol the moment the Job is created: it writes stdout, stderr and the
// exit status it was told to.
func newRunnerFixture(t *testing.T, stdout, stderr string, code int) *runnerFixture {
	t.Helper()
	root := t.TempDir()
	f := &runnerFixture{
		t:       t,
		cs:      fake.NewClientset(),
		root:    root,
		planID:  "0123456789ab",
		created: make(chan struct{}),
	}
	f.dir = filepath.Join(root, ".box", "migrate", f.planID, runnerDir)
	f.cs.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		job := action.(ktesting.CreateAction).GetObject().(*batchv1.Job)
		job.Name = job.GenerateName + "abcde"
		f.job = job.DeepCopy()
		f.answer(stdout, stderr, code)
		close(f.created)
		return false, nil, nil
	})
	// The Job completes as soon as it exists: the fake has no controller, and
	// what the runner waits for is the status a controller writes.
	f.cs.PrependReactor("get", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		job, err := f.cs.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), "seedbox", f.job.Name)
		if err != nil {
			return true, nil, err
		}
		got := job.(*batchv1.Job).DeepCopy()
		got.Status.Succeeded = 1
		got.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		return true, got, nil
	})
	f.runner = NewRunner(RunnerConfig{
		Client:     f.cs,
		Namespace:  "seedbox",
		Claim:      "seedbox-s5n-config",
		ConfigRoot: root,
		PlanID:     f.planID,
		Image:      "ghcr.io/home-operations/plex:1.43.5.11079@sha256:" + strings.Repeat("0", 64),
		PUID:       1234,
		PGID:       2345,
		Poll:       time.Millisecond,
		Timeout:    5 * time.Second,
	})
	return f
}

// answer is the fake pod: for every input file, it writes the output, the error
// text and the exit status, the way the shell's redirection would.
func (f *runnerFixture) answer(stdout, stderr string, code int) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		f.t.Fatalf("the runner did not leave its input where the pod can read it: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".in" {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".in")
		for ext, text := range map[string]string{".out": stdout, ".err": stderr, ".rc": string(rune('0'+code)) + "\n"} {
			if err := os.WriteFile(filepath.Join(f.dir, stem+ext), []byte(text), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}
	}
}

func TestRunnerJobSpec(t *testing.T) {
	f := newRunnerFixture(t, "rows", "", 0)
	if _, err := f.runner.Run(context.Background(), "plex", []string{"/usr/lib/plexmediaserver/Plex SQLite", "-bail", "/config/library.db"}, []byte("SELECT 1;\n")); err != nil {
		t.Fatalf("run: %v", err)
	}
	job := f.job
	if job == nil {
		t.Fatal("no job was created")
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("job spec: backoff %d, deadline %d; want 0 and 600", *job.Spec.BackoffLimit, *job.Spec.ActiveDeadlineSeconds)
	}
	pod := job.Spec.Template.Spec
	if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 1234 ||
		pod.SecurityContext.RunAsGroup == nil || *pod.SecurityContext.RunAsGroup != 2345 ||
		pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 2345 ||
		pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context is %+v, want the release's uid and gid under the restricted profile", pod.SecurityContext)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the pod carries a service account token")
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("pod has %d containers", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != f.runner.cfg.Image {
		t.Errorf("container image %q, want the release's pinned image %q", c.Image, f.runner.cfg.Image)
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation ||
		c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem ||
		c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot ||
		c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container security context is %+v, want no escalation, a read-only root, non-root and ALL dropped", c.SecurityContext)
	}
	// The command and its arguments: sh runs the script, argv[0] is $0 and the
	// rest are $1.. so the script can quote the image's own path.
	if strings.Join(c.Command, " ") != "sh -c "+c.Command[2] {
		t.Errorf("command is %q, want sh -c <script>", c.Command)
	}
	want := `"$0" "$@" </runner/plex-1.in >/runner/plex-1.out 2>/runner/plex-1.err; echo $? >/runner/plex-1.rc`
	if c.Command[2] != want {
		t.Errorf("script is %q, want %q", c.Command[2], want)
	}
	if strings.Join(c.Args, " ") != "/usr/lib/plexmediaserver/Plex SQLite -bail /config/library.db" {
		t.Errorf("args are %q, want the tool and its arguments", c.Args)
	}
	// Two mounts of the one claim: the staged config at /config and the
	// exchange directory at /runner, both subPaths of the plan's id.
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m.SubPath
	}
	if mounts["/config"] != filepath.Join(".box", "migrate", f.planID, "plex") {
		t.Errorf("/config is mounted at %q, want the plan's staged plex directory", mounts["/config"])
	}
	if mounts["/runner"] != filepath.Join(".box", "migrate", f.planID, runnerDir) {
		t.Errorf("/runner is mounted at %q, want the plan's exchange directory", mounts["/runner"])
	}
	if len(pod.Volumes) != 2 || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "seedbox-s5n-config" {
		t.Errorf("volumes are %+v, want the config claim and a scratch emptyDir", pod.Volumes)
	}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Error("the pod carries a hostPath: Pod Security restricted refuses it, and the claim is how the host directory is reached")
		}
	}

	// The admission policy admits it: its image is a blessed variant.
	repos, err := admission.Repositories([]byte(readChart(t, "variants.yaml")))
	if err != nil {
		t.Fatalf("read the blessed list: %v", err)
	}
	unblessed := admission.UnblessedImages([]admission.Pod{{
		Namespace:  "seedbox",
		Name:       "migrate",
		Containers: []admission.Container{{Name: c.Name, Image: c.Image}},
	}}, repos)
	if len(unblessed) != 0 {
		t.Errorf("the policy would refuse this pod: %v", unblessed)
	}
}

func TestRunnerFileProtocol(t *testing.T) {
	f := newRunnerFixture(t, "row|1|/data/media/tv\n", "", 0)
	out, err := f.runner.Run(context.Background(), "plex", []string{"/usr/lib/plexmediaserver/Plex SQLite", "/config/library.db"}, []byte("SELECT @tables;\n"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(out) != "row|1|/data/media/tv\n" {
		t.Errorf("stdout is %q, want what the tool wrote", out)
	}
	// Nothing is left behind: the Job, its pod's files and the input.
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the exchange directory still holds %d files", len(entries))
	}
	if _, err := f.cs.BatchV1().Jobs("seedbox").Get(context.Background(), f.job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the job is still there after a successful run: %v", err)
	}
}

func TestRunnerFailureCarriesStderr(t *testing.T) {
	f := newRunnerFixture(t, "", "Error: database is locked\n", 2)
	_, err := f.runner.Run(context.Background(), "plex", []string{"/usr/lib/plexmediaserver/Plex SQLite", "/config/library.db"}, nil)
	if err == nil {
		t.Fatal("a non-zero exit was accepted")
	}
	for _, want := range []string{"exited 2", "database is locked", "/usr/lib/plexmediaserver/Plex SQLite"} {
		if !contains(err.Error(), want) {
			t.Errorf("the error does not carry %q:\n%s", want, err)
		}
	}
	if _, err := f.cs.BatchV1().Jobs("seedbox").Get(context.Background(), f.job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the job is still there after a failed run: %v", err)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the exchange directory still holds %d files", len(entries))
	}
}

func TestRunnerCancelledRunDeletesItsJob(t *testing.T) {
	f := newRunnerFixture(t, "unused", "", 0)
	// The fake Job never completes here: its status stays pending, which is
	// what a pod that is still starting looks like. Once the run is over the
	// reactor steps aside, so the assertions below read the fake's own state.
	pending := true
	f.cs.PrependReactor("get", "jobs", func(ktesting.Action) (bool, runtime.Object, error) {
		if !pending {
			return false, nil, nil
		}
		return true, nil, context.Canceled
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		<-f.created
		cancel()
		close(done)
	}()
	_, err := f.runner.Run(ctx, "plex", []string{"/usr/lib/plexmediaserver/Plex SQLite", "/config/library.db"}, nil)
	<-done
	pending = false
	if err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if _, err := f.cs.BatchV1().Jobs("seedbox").Get(context.Background(), f.job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the job outlived the cancelled run: %v", err)
	}
}
