package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// install verifies one release and puts its binary where the running box is.
//
// The order is the safety: the signature over the checksums is checked before a
// line of them is read as a checksum; the binary is checked against that
// checksum before the old binary is copied aside; and both file writes are a
// temporary name and a rename, so the running box's path never names a file
// that is still being written.
func install(ctx context.Context, o Options, version, binary string, key ed25519.PublicKey) error {
	sums, err := get(ctx, o, o.assetURL(version, sumsAsset), sumsAsset, o.maxMetadataBytes())
	if err != nil {
		return err
	}
	signature, err := get(ctx, o, o.assetURL(version, sigAsset), sigAsset, o.maxMetadataBytes())
	if err != nil {
		return err
	}
	if err := VerifySums(key, version, sums, signature); err != nil {
		return err
	}

	name := binaryAsset()
	want, err := sumFor(sums, name)
	if err != nil {
		return err
	}

	dir := filepath.Dir(binary)
	staged, err := download(ctx, o, o.assetURL(version, name), name, dir, o.maxBinaryBytes())
	if err != nil {
		return err
	}
	// Nothing to remove once the rename below succeeds, and a failure before it
	// must not leave the partially written file behind.
	defer os.Remove(staged)

	// The digest is taken from the staged file, not from the bytes the download
	// counted: the file on disk is what will run, so a second read is what
	// proves the write landed intact.
	got, err := hashFile(staged)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("the release's %s is not the binary its %s names (sha256 %s, want %s): refusing to install it", name, sumsAsset, got, want)
	}

	// The old binary is kept before the new one lands, never renamed away:
	// between the two steps the path has to keep naming a working box.
	if err := keepPrevious(binary); err != nil {
		return err
	}
	if err := os.Rename(staged, binary); err != nil {
		return fmt.Errorf("installing over %s: %w", binary, err)
	}
	// The rename is what installs the new box; the directory sync is what makes
	// it survive a crash in the next few milliseconds. As in internal/atomicfile,
	// a failure here is not reported as a failed update: the bytes are already
	// in place under the new name.
	_ = syncDir(dir)
	return nil
}

// get fetches one small asset whole and refuses one over the cap. The cap is
// applied while reading, so a body that is not a release's two short files
// cannot fill memory first.
func get(ctx context.Context, o Options, url, name string, limit int64) ([]byte, error) {
	body, err := open(ctx, o, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the release's %s is larger than %d bytes, which no release's is: refusing it", name, limit)
	}
	return data, nil
}

// download writes one architecture's binary beside the binary it will replace
// and returns the temporary path. It sits in the same directory on purpose: the
// rename that installs it has to be a rename within one filesystem, and a full
// /tmp must not be able to fail the update halfway through.
func download(ctx context.Context, o Options, url, name, dir string, limit int64) (string, error) {
	body, err := open(ctx, o, url)
	if err != nil {
		return "", err
	}
	defer body.Close()

	file, err := os.CreateTemp(dir, ".box.new.")
	if err != nil {
		return "", fmt.Errorf("writing the new box beside %s: %w", dir, err)
	}
	staged := file.Name()
	fail := func(err error) (string, error) {
		file.Close()
		os.Remove(staged)
		return "", err
	}
	n, err := io.Copy(file, io.LimitReader(body, limit+1))
	if err != nil {
		return fail(fmt.Errorf("downloading %s: %w", url, err))
	}
	if n > limit {
		return fail(fmt.Errorf("the release's %s is larger than %d bytes, which no box release's is: refusing it", name, limit))
	}
	// The bytes have to be durable before the name that makes them the running
	// box is: an update that survives the rename but not a power cut is a box
	// that could come back as a half-written file.
	if err := file.Sync(); err != nil {
		return fail(fmt.Errorf("writing the new box beside %s: %w", dir, err))
	}
	if err := file.Chmod(0o755); err != nil {
		return fail(fmt.Errorf("writing the new box beside %s: %w", dir, err))
	}
	if err := file.Close(); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("writing the new box beside %s: %w", dir, err)
	}
	return staged, nil
}

// open starts one asset download. internal/fetch's transport has already
// retried a transient failure and named the URL in its own error, so an error
// here is reported as it comes.
func open(ctx context.Context, o Options, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := assetClient(o).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		discard(resp.Body)
		return nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// hashFile is the sha256 of a file on disk, hex, as a release's checksums spell
// it.
func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("reading back the new box: %w", err)
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", fmt.Errorf("reading back the new box: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// keepPrevious copies the running binary to box.previous, through a temporary
// file and a rename so a failure leaves no half-written copy.
//
// It streams rather than going through internal/atomicfile, which takes the
// bytes in memory: a box binary is tens of megabytes and there is no reason to
// hold two copies of it to keep one.
func keepPrevious(binary string) error {
	previous := previousPath(binary)
	src, err := os.Open(binary)
	if err != nil {
		return fmt.Errorf("keeping a copy of the running box: %w", err)
	}
	defer src.Close()

	dir := filepath.Dir(binary)
	file, err := os.CreateTemp(dir, "."+filepath.Base(previous)+".tmp-")
	if err != nil {
		return fmt.Errorf("keeping a copy of the running box: %w", err)
	}
	staged := file.Name()
	fail := func(err error) error {
		file.Close()
		os.Remove(staged)
		return err
	}
	if _, err := io.Copy(file, src); err != nil {
		return fail(fmt.Errorf("keeping a copy of the running box: %w", err))
	}
	if err := file.Sync(); err != nil {
		return fail(fmt.Errorf("keeping a copy of the running box: %w", err))
	}
	if err := file.Chmod(0o755); err != nil {
		return fail(fmt.Errorf("keeping a copy of the running box: %w", err))
	}
	if err := file.Close(); err != nil {
		os.Remove(staged)
		return fmt.Errorf("keeping a copy of the running box: %w", err)
	}
	if err := os.Rename(staged, previous); err != nil {
		os.Remove(staged)
		return fmt.Errorf("keeping a copy of the running box at %s: %w", previous, err)
	}
	_ = syncDir(dir)
	return nil
}

// syncDir makes a rename in dir durable: the bytes were fsynced, but the name
// that points at them lives in the directory.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}

// discard drains and closes a body box is not going to read, so the connection
// goes back to the pool instead of being torn down. (internal/fetch keeps its
// own unexported copy for the same reason.)
func discard(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}
