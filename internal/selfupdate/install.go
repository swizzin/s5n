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
// The order is the safety: signature before any checksum is read, checksum
// before the old binary is copied aside, and every write is a temporary name
// renamed into place.
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
	// No-op after the rename; removes the staged file on any earlier failure.
	defer os.Remove(staged)

	// Hash the staged file, not the download count: the disk is what will run.
	got, err := hashFile(staged)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("the release's %s is not the binary its %s names (sha256 %s, want %s): refusing to install it", name, sumsAsset, got, want)
	}

	// Keep the old binary before the new one lands: the path must always name
	// a working box.
	if err := keepPrevious(binary); err != nil {
		return err
	}
	if err := os.Rename(staged, binary); err != nil {
		return fmt.Errorf("installing over %s: %w", binary, err)
	}
	// The rename installs the new box; the directory sync makes it survive a
	// crash. A failed sync is not a failed update: the bytes are in place.
	_ = syncDir(dir)
	return nil
}

// get fetches one small asset whole, refusing one over the cap while reading.
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

// download writes the binary beside the one it will replace and returns the
// temporary path: the rename must stay within one filesystem, and a full /tmp
// must not fail the update halfway.
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
	// The bytes must be durable before the rename makes them the running box.
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

// open starts one asset download; internal/fetch has already retried and named
// the URL.
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

// hashFile is a file's sha256 in hex, as a release's checksums spell it.
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

// keepPrevious copies the running binary to box.previous through a temporary
// file and a rename. It streams rather than using internal/atomicfile, which
// would hold two copies of a tens-of-megabytes binary in memory.
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

// syncDir makes a rename durable: the name lives in the directory.
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

// discard drains and closes an unread body so the connection is reused.
// internal/fetch keeps its own copy for the same reason.
func discard(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}
