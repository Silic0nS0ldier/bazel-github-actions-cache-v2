package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

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

// degrade reports a backend failure as a miss when the server is fail-open, so
// that a cache outage slows a build down instead of breaking it.
func (s *Server) degrade(failure error) error {
	if s.cfg.FailOpen {
		s.stats.misses.Add(1)
		return errCacheMiss
	}
	return failure
}

// readObject resolves an object and, for action results, validates that its CAS
// closure is still complete.
func (s *Server) readObject(ctx context.Context, kind, digest string) (object, error) {
	s.stats.operations.Add(1)
	obj, found, err := s.resolve(ctx, s.objectKey(kind, digest), kind, digest)
	if err != nil {
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("backend load failed for %s/%s: %s", kind, digest, safeError(err))
		return object{}, s.degrade(errBackendFailure)
	}
	if !found {
		s.stats.misses.Add(1)
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
// returned file. A successful call counts a cache hit.
func (s *Server) openObject(ctx context.Context, kind, digest string) (*os.File, int64, error) {
	obj, err := s.readObject(ctx, kind, digest)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(obj.path)
	if err != nil {
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("open local cache object %s/%s: %v", kind, digest, err)
		if s.cfg.FailOpen {
			s.stats.misses.Add(1)
			return nil, 0, errCacheMiss
		}
		return nil, 0, errLocalFailure
	}
	s.stats.hits.Add(1)
	return file, obj.size, nil
}

// casPresence answers a presence probe from the manifest view when the storage
// mode allows it. Bazel issues these while building without needing the bytes,
// so restoring a whole pack to answer one would defeat the point.
func (s *Server) casPresence(ctx context.Context, digest string) (int64, error) {
	s.stats.operations.Add(1)
	size, found, err := s.presence(ctx, s.objectKey("cas", digest))
	if err != nil {
		s.cfg.Logger.Printf("presence check for cas/%s failed: %s", digest, safeError(err))
	}
	if err != nil || !found {
		s.stats.misses.Add(1)
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
	if _, err := io.ReadFull(file, data); err != nil {
		s.cfg.Logger.Printf("read local cache object %s/%s: %v", kind, digest, err)
		return nil, errLocalFailure
	}
	s.stats.bytesServed.Add(uint64(len(data)))
	return data, nil
}

func (s *Server) validateStoredActionResult(ctx context.Context, digest string, obj object) error {
	err := s.validateActionResult(ctx, obj)
	switch {
	case err == nil:
		s.stats.validatedActionResults.Add(1)
		return nil
	case errors.Is(err, errIncompleteActionResult):
		s.stats.incompleteActionResults.Add(1)
		s.stats.misses.Add(1)
		s.cfg.Logger.Printf("action result ac/%s is incomplete: %s", digest, safeError(err))
		return errCacheMiss
	case errors.Is(err, errInvalidActionResult):
		s.stats.invalidActionResults.Add(1)
		s.stats.misses.Add(1)
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
	file, err := os.CreateTemp(s.cfg.CacheDir, "upload-*")
	if err != nil {
		s.cfg.Logger.Printf("create upload spool for %s/%s: %v", kind, digest, err)
		return errLocalFailure
	}
	path := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()

	hasher := sha256.New()
	writer := io.Writer(file)
	if kind == "cas" {
		writer = io.MultiWriter(file, hasher)
	}
	n, err := io.Copy(writer, io.LimitReader(body, size+1))
	s.stats.bytesReceived.Add(uint64(n))
	if err != nil || n != size {
		return errPayloadTruncated
	}
	if kind == "cas" && hex.EncodeToString(hasher.Sum(nil)) != digest {
		return errPayloadMismatch
	}
	if err := file.Sync(); err != nil {
		s.cfg.Logger.Printf("sync upload spool for %s/%s: %v", kind, digest, err)
		return errLocalFailure
	}

	key := s.objectKey(kind, digest)
	s.objectsMu.Lock()
	stored, exists := s.objects[key]
	if !exists {
		stored = object{path: path, size: n}
		s.objects[key] = stored
		keep = true
	}
	s.objectsMu.Unlock()

	if !s.cfg.WriteEnabled {
		s.stats.discardedUploads.Add(1)
		return nil
	}
	if s.packs != nil {
		return s.stagePacked(ctx, kind, digest, stored)
	}
	if kind == "ac" {
		if err := s.validateActionResultForPublication(ctx, object{path: path, size: n}); err != nil {
			return s.absorbActionResultFailure(digest, "publishing", err)
		}
		s.stats.validatedActionResults.Add(1)
	}

	deduplicated, err := s.publishOnce(ctx, key, file, n)
	if err != nil {
		return s.absorbSaveFailure(kind, digest, err)
	}
	if deduplicated {
		s.stats.deduplicatedUploads.Add(1)
	} else {
		s.stats.uploads.Add(1)
	}
	return nil
}

// stagePacked hands an upload to the CARv2 pack writer, which publishes it as
// part of a later batch rather than as its own backend entry.
func (s *Server) stagePacked(ctx context.Context, kind, digest string, stored object) error {
	if kind == "cas" {
		s.packs.stageCAS(digest, stored)
		return nil
	}
	closure, err := s.collectActionResultClosure(ctx, stored)
	if err != nil {
		return s.absorbActionResultFailure(digest, "staging", err)
	}
	if err := s.packs.stageAction(digest, stored, closure); err != nil {
		return s.absorbSaveFailure(kind, digest, err)
	}
	s.stats.validatedActionResults.Add(1)
	return nil
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
