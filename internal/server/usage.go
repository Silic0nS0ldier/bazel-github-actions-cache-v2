package server

import (
	"encoding/json"
	"sort"
	"sync"
)

// Access classes recorded for a cache entry.
const (
	accessPresence = "presence"
	accessDownload = "download"
)

// UsageEntry is one cache entry a job touched.
type UsageEntry struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Pack   string `json:"pack,omitempty"`
	// Absent when nothing reported a size. Zero is a real size, not a missing
	// one: the empty blob has a well-known digest.
	Size           *int64 `json:"size,omitempty"`
	PresenceChecks uint64 `json:"presence_checks"`
	Downloads      uint64 `json:"downloads"`
}

// Access reports how the entry was used. A presence-only entry still has to
// exist, or the action result referencing it becomes a miss, but nothing ever
// needed its bytes.
func (e UsageEntry) Access() string {
	if e.Downloads > 0 {
		return accessDownload
	}
	return accessPresence
}

// UsagePack describes how much of a restored pack a job turned out to need.
type UsagePack struct {
	ID        string `json:"id"`
	Size      int64  `json:"size"`
	BytesUsed int64  `json:"bytes_used"`
	Restored  bool   `json:"restored"`
}

// UsageReport is the record an optimisation pass consumes. It is deliberately
// about what was used rather than what exists: pack composition can be read back
// from the manifests, but only a job knows which entries it needed.
type UsageReport struct {
	Entries           []UsageEntry `json:"entries"`
	Packs             []UsagePack  `json:"packs"`
	PackBytesRestored int64        `json:"pack_bytes_restored"`
	PackBytesUsed     int64        `json:"pack_bytes_used"`
}

func (r UsageReport) JSON() []byte {
	data, _ := json.Marshal(r)
	return data
}

type usageRecorder struct {
	mu       sync.Mutex
	entries  map[string]*UsageEntry
	restored map[string]int64
}

func newUsageRecorder() *usageRecorder {
	return &usageRecorder{
		entries:  make(map[string]*UsageEntry),
		restored: make(map[string]int64),
	}
}

func (r *usageRecorder) record(kind, digest, pack, access string, size int64) {
	if digest == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, found := r.entries[kind+"/"+digest]
	if !found {
		entry = &UsageEntry{Kind: kind, Digest: digest}
		r.entries[kind+"/"+digest] = entry
	}
	if pack != "" {
		entry.Pack = pack
	}
	// A presence check in objects mode reports no size, so never let that erase
	// a size a download established.
	if size >= 0 {
		recorded := size
		entry.Size = &recorded
	}
	if access == accessDownload {
		entry.Downloads++
		return
	}
	entry.PresenceChecks++
}

// packRestored notes that a pack was paid for. The bytes it cost are compared
// against the bytes that turned out to be wanted, which is the ratio that says
// whether packing is placing hot and cold content together.
func (r *usageRecorder) packRestored(pack string, size int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restored[pack] = size
}

func (r *usageRecorder) report() UsageReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	report := UsageReport{
		Entries: make([]UsageEntry, 0, len(r.entries)),
		Packs:   make([]UsagePack, 0, len(r.restored)),
	}
	used := make(map[string]int64, len(r.restored))
	for _, entry := range r.entries {
		copied := *entry
		if entry.Size != nil {
			size := *entry.Size
			copied.Size = &size
		}
		report.Entries = append(report.Entries, copied)
		// Only a download consumes the bandwidth a restore paid for; a presence
		// check is answered from the manifest without touching the pack.
		if copied.Pack != "" && copied.Downloads > 0 && copied.Size != nil {
			used[copied.Pack] += *copied.Size
		}
	}
	for pack, size := range r.restored {
		report.Packs = append(report.Packs, UsagePack{
			ID:        pack,
			Size:      size,
			BytesUsed: used[pack],
			Restored:  true,
		})
		report.PackBytesRestored += size
		report.PackBytesUsed += used[pack]
	}

	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].Kind != report.Entries[j].Kind {
			return report.Entries[i].Kind < report.Entries[j].Kind
		}
		return report.Entries[i].Digest < report.Entries[j].Digest
	})
	sort.Slice(report.Packs, func(i, j int) bool { return report.Packs[i].ID < report.Packs[j].ID })
	return report
}
