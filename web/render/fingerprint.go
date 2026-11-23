package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// fingerprintLength is how many hex chars of the SHA-256 we keep in
// each asset URL. 8 chars = 32 bits of distinct values, which is
// plenty for cache busting — we only care about "is this content
// different from before", not about adversarial collisions. Shorter
// hashes also keep rendered HTML compact.
const fingerprintLength = 8

// fsSub is the seam tests swap to force the fs.Sub error branch in
// loadFingerprints (and Mount). For the constant name "static" the
// real fs.Sub never errors with stdlib FS impls — without the seam
// the defensive guard would stay permanently uncovered.
var fsSub = fs.Sub

// loadFingerprints walks the static/ subtree of s.fs, computes the
// SHA-256 of every file, and stores the first fingerprintLength hex
// chars in s.fingerprints keyed by the file's logical path within
// static/ (e.g. "css/base.css", "img/hero.jpg"). The asset() template
// helper consults this map to emit content-stable URLs that pair
// safely with `Cache-Control: immutable`:
//
//   - same content → same hash → same URL → browser cache hit;
//   - any byte changes → new hash → new URL → forced refetch.
//
// Hashes are deterministic across boots and across pods (same input,
// same SHA-256), so two replicas of the same binary emit identical
// URLs and clients hit a CDN cache without coordination.
//
// In dev mode (Site rebuilt per request) this re-runs on every
// request. For typical sites with a handful of small static files
// the walk is sub-millisecond — acceptable for the simplicity gain
// of "no separate manifest file, no build step, just works".
//
// A missing static/ directory is not an error: services that don't
// ship embedded assets (a JSON API, a NATS consumer) end up with an
// empty map and asset() falls back to the boot-time assetVer.
func (s *Site) loadFingerprints() error {
	s.fingerprints = map[string]string{}

	if err := s.fingerprintLocalStatic(); err != nil {
		return err
	}

	// Fingerprint each ExtraStatic subtree under its prefix so the
	// asset helper resolves "<prefix>/<file>" the same way it does
	// service-local paths. Done unconditionally — a service that
	// ships no own static (a JSON API plugged into uikit, say) still
	// needs its shared assets fingerprinted.
	for prefix, fsys := range s.extraStatic {
		if err := walkFingerprints(fsys, prefix+"/", s.fingerprints); err != nil {
			return fmt.Errorf("extra static %q: %w", prefix, err)
		}
	}
	return nil
}

// fingerprintLocalStatic handles the service's own static/ subtree:
// the missing-directory branches that a JSON-only service hits, plus
// the actual walk. Pulled out of loadFingerprints so the orchestration
// in loadFingerprints reads "fingerprint local, then fingerprint
// extras" without nested early returns.
func (s *Site) fingerprintLocalStatic() error {
	staticSub, err := fsSub(s.fs, "static")
	if err != nil {
		// fs.Sub itself can't fail for a constant name, but we keep
		// the guard so a future fs.FS implementation that returns an
		// error here doesn't crash boot.
		return nil
	}
	if _, err := fs.Stat(staticSub, "."); err != nil {
		// No static/ in this site: nothing to fingerprint.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat static/: %w", err)
	}
	return walkFingerprints(staticSub, "", s.fingerprints)
}

// walkFingerprints walks fsys from "." and writes (keyPrefix+path →
// hash) entries into out. Pulled out of loadFingerprints so the
// service-local static walk and each ExtraStatic walk share the
// exact same hashing + key-derivation rules — a divergence between
// the two is the kind of bug that breaks cache busting silently.
func walkFingerprints(fsys fs.FS, keyPrefix string, out map[string]string) error {
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		hash, err := hashFile(fsys, path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", path, err)
		}
		// fs.FS spec mandates forward-slash paths regardless of OS,
		// so the key is already URL-shaped — but trim a leading "./"
		// that some FS implementations include for the walk root.
		key := keyPrefix + strings.TrimPrefix(path, "./")
		out[key] = hash
		return nil
	})
}

// hashFile reads the file at path inside fsys and returns the first
// fingerprintLength hex chars of its SHA-256. Streamed via io.Copy
// so a large asset (image, font) never doubles the binary's RAM
// footprint at boot.
func hashFile(fsys fs.FS, path string) (string, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if len(sum) > fingerprintLength {
		sum = sum[:fingerprintLength]
	}
	return sum, nil
}
