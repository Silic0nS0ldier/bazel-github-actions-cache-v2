package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func usageFor(t *testing.T, report UsageReport, kind, digest string) UsageEntry {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.Kind == kind && entry.Digest == digest {
			return entry
		}
	}
	t.Fatalf("no usage recorded for %s/%s in %+v", kind, digest, report.Entries)
	return UsageEntry{}
}

func TestUsageSeparatesPresenceChecksFromDownloads(t *testing.T) {
	downloaded := []byte("an output something actually reads")
	downloadedReference := referenceFor(downloaded)
	backend := newMemoryBackend()
	backend.objects["test-v1-cas-"+downloadedReference.hash] = downloaded
	server := testServer(t, backend, nil)

	headCacheObject(server, "/cas/"+downloadedReference.hash)
	readCacheObject(server, "/cas/"+downloadedReference.hash)

	entry := usageFor(t, server.Usage(), "cas", downloadedReference.hash)
	if entry.Downloads == 0 || entry.Access() != accessDownload {
		t.Fatalf("entry = %+v, want a download", entry)
	}
	if entry.Size == nil || *entry.Size != downloadedReference.size {
		t.Fatalf("size = %v, want %d", entry.Size, downloadedReference.size)
	}
}

// An output that is only ever presence-checked still has to exist, or the action
// result referencing it becomes a miss. Recording those checks is what stops a
// later optimisation pass from treating such an output as dead.
func TestUsageRecordsClosureValidationPresenceChecks(t *testing.T) {
	output := []byte("an output nothing downloads")
	outputReference := referenceFor(output)
	actionDigest := strings.Repeat("a", 64)
	backend := newMemoryBackend()
	backend.objects["test-v1-cas-"+outputReference.hash] = output
	backend.objects["test-v1-ac-"+actionDigest] = actionResultProto(t, outputReference)
	server := testServer(t, backend, nil)

	if response := readCacheObject(server, "/ac/"+actionDigest); response.Code != http.StatusOK {
		t.Fatalf("AC GET = %d", response.Code)
	}

	report := server.Usage()
	action := usageFor(t, report, "ac", actionDigest)
	if action.Downloads == 0 {
		t.Fatalf("action result = %+v, want a download", action)
	}
	blob := usageFor(t, report, "cas", outputReference.hash)
	if blob.PresenceChecks == 0 || blob.Access() != accessPresence {
		t.Fatalf("output = %+v, want a presence check and no download", blob)
	}
	// Objects mode answers a presence check without learning a size. Reporting
	// zero there would name the empty blob, which has a well-known digest.
	if blob.Size != nil {
		t.Fatalf("size = %d, want none", *blob.Size)
	}
	encoded, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"size"`) {
		t.Fatalf("entry names a size it does not know: %s", encoded)
	}
}

// The ratio of bytes restored to bytes wanted is what says whether packing is
// putting frequently and rarely fetched content together.
func TestUsageReportsPackYield(t *testing.T) {
	hot := []byte(strings.Repeat("h", 300))
	cold := []byte(strings.Repeat("c", 300))
	hotReference := referenceFor(hot)
	coldReference := referenceFor(cold)
	backend := newMemoryBackend()

	packed := func(t *testing.T) *Server {
		t.Helper()
		return testServer(t, backend, func(cfg *Config) {
			cfg.StorageMode = "packs"
			cfg.Catalog = memoryCatalog{backend: backend}
			cfg.PackSize = 1024
			cfg.PackFlushInterval = time.Hour
			cfg.MaxManifests = 16
		})
	}

	seed := packed(t)
	for _, blob := range [][]byte{hot, cold} {
		if response := putCacheObject(seed, "/cas/"+digest(blob), blob); response.Code != http.StatusNoContent {
			t.Fatalf("CAS PUT = %d", response.Code)
		}
	}
	closePackedServer(t, seed)

	// A fresh server pays for the whole pack to read one of the two blobs.
	restored := packed(t)
	if response := readCacheObject(restored, "/cas/"+hotReference.hash); response.Code != http.StatusOK {
		t.Fatalf("CAS GET = %d", response.Code)
	}
	report := restored.Usage()
	if len(report.Packs) != 1 {
		t.Fatalf("packs = %+v, want exactly one restored pack", report.Packs)
	}
	if report.PackBytesUsed != hotReference.size {
		t.Fatalf("bytes used = %d, want %d", report.PackBytesUsed, hotReference.size)
	}
	if report.PackBytesRestored <= report.PackBytesUsed {
		t.Fatalf("restored %d bytes to use %d; the yield should be below 1",
			report.PackBytesRestored, report.PackBytesUsed)
	}
	if pack := usageFor(t, report, "cas", hotReference.hash).Pack; pack != report.Packs[0].ID {
		t.Fatalf("entry pack = %q, want %q", pack, report.Packs[0].ID)
	}
	// The cold blob was never asked for, so it must not appear as used.
	for _, entry := range report.Entries {
		if entry.Digest == coldReference.hash {
			t.Fatalf("unused blob was recorded: %+v", entry)
		}
	}

	stats := restored.Snapshot()
	if stats.PackBytesRestored != uint64(report.PackBytesRestored) ||
		stats.PackBytesUsed != uint64(report.PackBytesUsed) {
		t.Fatalf("stats disagree with the usage report: %+v", stats)
	}
}
