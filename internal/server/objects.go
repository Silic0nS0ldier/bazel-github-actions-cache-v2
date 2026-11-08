package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
)

type lockedObjectReader struct {
	*os.File
	unlock func()
	once   sync.Once
}

func (r *lockedObjectReader) Close() error {
	err := r.File.Close()
	r.once.Do(r.unlock)
	return err
}

type readSeekCloser struct {
	*bytes.Reader
}

func (readSeekCloser) Close() error { return nil }

func (o object) open() (io.ReadSeekCloser, error) {
	if o.inMemory {
		return readSeekCloser{Reader: bytes.NewReader(o.data)}, nil
	}
	return os.Open(o.path)
}

func (o object) readAll() ([]byte, error) {
	if o.inMemory {
		return bytes.Clone(o.data), nil
	}
	return os.ReadFile(o.path)
}

// Transport-neutral outcomes of a cache operation. Cache policy — the fail-open
// decision, statistics, and redacted logging — is applied by the operations
// below so that every transport behaves identically; a transport only maps these
// onto its own status vocabulary.
var (
	errCacheMiss        = errors.New("cache miss")
	errBackendFailure   = errors.New("cache backend unavailable")
	errLocalFailure     = errors.New("local cache unavailable")
	errPayloadMismatch  = errors.New("payload does not match the requested digest")
	errPayloadTruncated = errors.New("payload size does not match the declared length")
)

func (s *Server) objectKey(kind, digest string) string {
	return s.cfg.KeyPrefix + "-" + kind + "-" + digest
}

// recordMiss counts a miss against both the headline total and the bucket that
// explains it, so the breakdown can never drift from the total.
func (s *Server) recordMiss(bucket *atomic.Uint64) {
	s.stats.misses.Add(1)
	bucket.Add(1)
}

// degrade reports a backend failure as a miss when the server is fail-open, so
// that a cache outage slows a build down instead of breaking it.
func (s *Server) degrade(failure error) error {
	if s.cfg.FailOpen {
		s.recordMiss(&s.stats.missesDegraded)
		return errCacheMiss
	}
	return failure
}

// readObject resolves an object and, for action results, validates that its CAS
// closure is still complete.
func (s *Server) readObject(ctx context.Context, kind, digest string) (object, error) {
	s.objectFilesMu.RLock()
	defer s.objectFilesMu.RUnlock()
	return s.readObjectWithObjectFilesLock(ctx, kind, digest)
}

func (s *Server) readObjectWithObjectFilesLock(ctx context.Context, kind, digest string) (object, error) {
	s.stats.operations.Add(1)
	obj, found, err := s.resolve(ctx, s.objectKey(kind, digest), kind, digest)
	if err != nil {
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("backend load failed for %s/%s: %s", kind, digest, safeError(err))
		return object{}, s.degrade(errBackendFailure)
	}
	if !found {
		if kind == "ac" {
			s.recordMiss(&s.stats.missesAC)
		} else {
			s.recordMiss(&s.stats.missesCAS)
		}
		return object{}, errCacheMiss
	}
	if kind == "ac" {
		if err := s.validateStoredActionResult(ctx, digest, obj); err != nil {
			return object{}, err
		}
	}
	return obj, nil
}

// openObject resolves an object and opens it for serving. The caller closes the
// returned reader. A successful call counts a cache hit.
func (s *Server) openObject(ctx context.Context, kind, digest string) (io.ReadSeekCloser, int64, error) {
	s.objectFilesMu.RLock()
	unlock := true
	defer func() {
		if unlock {
			s.objectFilesMu.RUnlock()
		}
	}()
	obj, err := s.readObjectWithObjectFilesLock(ctx, kind, digest)
	if err != nil {
		return nil, 0, err
	}
	var reader io.ReadSeekCloser
	if obj.inMemory {
		reader, err = obj.open()
	} else {
		err = s.fileIO.do(ctx, func() (innerErr error) {
			reader, innerErr = obj.open()
			return innerErr
		})
	}
	if err != nil {
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("open local cache object %s/%s: %v", kind, digest, err)
		if s.cfg.FailOpen {
			s.recordMiss(&s.stats.missesDegraded)
			return nil, 0, errCacheMiss
		}
		return nil, 0, errLocalFailure
	}
	if file, ok := reader.(*os.File); ok && runtime.GOOS == "windows" {
		// Windows cannot unlink an open spool file. Keep the deletion lock
		// until the transport closes its reader, not just until it opens it.
		reader = &lockedObjectReader{File: file, unlock: s.objectFilesMu.RUnlock}
		unlock = false
	}
	s.stats.hits.Add(1)
	return reader, obj.size, nil
}

// casPresence answers a presence probe from the manifest view when the storage
// mode allows it. Bazel issues these while building without needing the bytes,
// so restoring a whole pack to answer one would defeat the point.
func (s *Server) casPresence(ctx context.Context, digest string) (int64, error) {
	s.stats.operations.Add(1)
	size, found, err := s.presence(ctx, "cas", digest)
	if err != nil {
		s.cfg.Logger.Printf("presence check for cas/%s failed: %s", digest, safeError(err))
	}
	if err != nil || !found {
		s.recordMiss(&s.stats.missesPresence)
		return 0, errCacheMiss
	}
	s.stats.hits.Add(1)
	return size, nil
}

// implicitEmptyCASHit records the zero-byte CAS digest, which every transport
// answers from the digest alone without consulting storage.
func (s *Server) implicitEmptyCASHit() {
	s.stats.operations.Add(1)
	s.stats.hits.Add(1)
}

// readObjectBytes returns a whole object. Callers are responsible for bounding
// the size beforehand, so it suits action results, directories, and batched
// blobs rather than arbitrary CAS reads.
func (s *Server) readObjectBytes(ctx context.Context, kind, digest string) ([]byte, error) {
	file, size, err := s.openObject(ctx, kind, digest)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data := make([]byte, size)
	if _, err := io.ReadFull(s.gateReader(ctx, file), data); err != nil {
		s.cfg.Logger.Printf("read local cache object %s/%s: %v", kind, digest, err)
		return nil, errLocalFailure
	}
	s.stats.bytesServed.Add(uint64(len(data)))
	return data, nil
}

func (s *Server) gateReader(ctx context.Context, reader io.Reader) io.Reader {
	switch reader.(type) {
	case *os.File, *lockedObjectReader:
		return gatedReader{ctx: ctx, gate: s.fileIO, r: reader}
	}
	return reader
}

func (s *Server) validateStoredActionResult(ctx context.Context, digest string, obj object) error {
	err := s.validateActionResult(ctx, obj)
	switch {
	case err == nil:
		s.stats.validatedActionResults.Add(1)
		return nil
	case errors.Is(err, errIncompleteActionResult):
		s.stats.incompleteActionResults.Add(1)
		s.recordMiss(&s.stats.missesRejected)
		s.cfg.Logger.Printf("action result ac/%s is incomplete: %s", digest, safeError(err))
		return errCacheMiss
	case errors.Is(err, errInvalidActionResult):
		s.stats.invalidActionResults.Add(1)
		s.recordMiss(&s.stats.missesRejected)
		s.cfg.Logger.Printf("action result ac/%s is invalid: %s", digest, safeError(err))
		return errCacheMiss
	default:
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("action result ac/%s validation failed: %s", digest, safeError(err))
		return s.degrade(errBackendFailure)
	}
}

// writeObject spools a payload, verifies it against the requested digest, and
// publishes it when writes are enabled. A nil error only means the caller's
// request was well formed: cache-side failures are absorbed by the fail-open
// policy and reported through the statistics instead.
func (s *Server) writeObject(
	ctx context.Context,
	kind, digest string,
	body io.Reader,
	size int64,
) error {
	s.stats.operations.Add(1)
	var file *os.File
	err := s.fileIO.do(ctx, func() (err error) {
		file, err = os.CreateTemp(s.cfg.CacheDir, "upload-*")
		return err
	})
	if err != nil {
		s.cfg.Logger.Printf("create upload spool for %s/%s: %v", kind, digest, err)
		return errLocalFailure
	}
	path := file.Name()
	keep := false
	defer func() {
		_ = s.fileIO.do(context.Background(), func() error {
			_ = file.Close()
			if !keep {
				_ = os.Remove(path)
			}
			return nil
		})
	}()

	hasher := sha256.New()
	writer := io.Writer(gatedWriter{ctx: ctx, gate: s.fileIO, w: file})
	if kind == "cas" {
		writer = io.MultiWriter(writer, hasher)
	}
	n, err := io.Copy(writer, io.LimitReader(body, size+1))
	s.stats.bytesReceived.Add(uint64(n))
	if err != nil || n != size {
		return errPayloadTruncated
	}
	if kind == "cas" && hex.EncodeToString(hasher.Sum(nil)) != digest {
		return errPayloadMismatch
	}
	keep, err = s.storeObjectFile(ctx, kind, digest, file, path, n)
	return err
}

// storeObjectFile retains a verified spool locally and sends it through the
// normal publication path without copying or hashing it again.
func (s *Server) storeObjectFile(
	ctx context.Context,
	kind, digest string,
	file *os.File,
	path string,
	size int64,
) (bool, error) {
	keep := false
	packed := s.packs != nil
	if packed && runtime.GOOS == "windows" {
		// Publication reads packed objects by path. Close the upload handle
		// before staging can wake a worker that removes the original spool.
		if err := s.fileIO.do(ctx, file.Close); err != nil {
			s.cfg.Logger.Printf("close upload spool for %s/%s: %v", kind, digest, err)
			return false, errLocalFailure
		}
	}
	if packed {
		s.objectFilesMu.RLock()
	}
	key := s.objectKey(kind, digest)
	s.objectsMu.Lock()
	stored, exists := s.objects[key]
	if !exists {
		stored = object{path: path, size: size}
		s.objects[key] = stored
		keep = true
	}
	s.objectsMu.Unlock()

	if !s.cfg.WriteEnabled {
		if packed {
			s.objectFilesMu.RUnlock()
		}
		s.stats.discardedUploads.Add(1)
		return keep, nil
	}
	if packed {
		retained, err := s.stagePacked(ctx, kind, digest, stored)
		s.objectFilesMu.RUnlock()
		if !retained {
			s.discardPackedObject(key, stored.path)
			if stored.path == path {
				keep = false
			}
		}
		return keep, err
	}
	if kind == "ac" {
		if err := s.validateActionResultForPublication(ctx, object{path: path, size: size}); err != nil {
			return keep, s.absorbActionResultFailure(digest, "publishing", err)
		}
		s.stats.validatedActionResults.Add(1)
	}

	deduplicated, err := s.publishOnce(ctx, key, file, size)
	if err != nil {
		return keep, s.absorbSaveFailure(kind, digest, err)
	}
	if deduplicated {
		s.stats.deduplicatedUploads.Add(1)
	} else {
		s.stats.uploads.Add(1)
	}
	return keep, nil
}

// stagePacked hands an upload to the CARv2 pack writer, which publishes it as
// part of a later batch rather than as its own backend entry.
func (s *Server) stagePacked(ctx context.Context, kind, digest string, stored object) (bool, error) {
	if kind == "cas" {
		return s.packs.stageCAS(digest, stored), nil
	}
	closure, err := s.collectActionResultClosure(ctx, stored)
	if err != nil {
		return true, s.absorbActionResultFailure(digest, "staging", err)
	}
	if err := s.packs.stageAction(digest, stored, closure); err != nil {
		return true, s.absorbSaveFailure(kind, digest, err)
	}
	s.stats.validatedActionResults.Add(1)
	return true, nil
}

func (s *Server) discardPackedObject(key, path string) {
	s.objectFilesMu.Lock()
	defer s.objectFilesMu.Unlock()
	s.objectsMu.Lock()
	if current, exists := s.objects[key]; exists && current.path == path {
		delete(s.objects, key)
		s.objectsMu.Unlock()
		s.packs.removeLocalFile(path)
		return
	}
	s.objectsMu.Unlock()
}

// absorbActionResultFailure drops an action result that must not be published.
// The upload still succeeds from the client's point of view, because a build
// should not fail merely because its result was not cacheable.
func (s *Server) absorbActionResultFailure(digest, stage string, err error) error {
	switch {
	case errors.Is(err, errIncompleteActionResult):
		s.stats.incompleteActionResults.Add(1)
		s.stats.skippedActionResultUploads.Add(1)
		s.cfg.Logger.Printf("not %s incomplete action result ac/%s: %s", stage, digest, safeError(err))
	case errors.Is(err, errInvalidActionResult):
		s.stats.invalidActionResults.Add(1)
		s.stats.skippedActionResultUploads.Add(1)
		s.cfg.Logger.Printf("not %s invalid action result ac/%s: %s", stage, digest, safeError(err))
	default:
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("action result ac/%s failed validation while %s: %s", digest, stage, safeError(err))
		if !s.cfg.FailOpen {
			return errBackendFailure
		}
		s.stats.skippedActionResultUploads.Add(1)
	}
	return nil
}

func (s *Server) absorbSaveFailure(kind, digest string, err error) error {
	s.stats.backendSaveErrors.Add(1)
	s.cfg.Logger.Printf("backend save failed for %s/%s: %s", kind, digest, safeError(err))
	if s.cfg.FailOpen {
		return nil
	}
	return errBackendFailure
}
