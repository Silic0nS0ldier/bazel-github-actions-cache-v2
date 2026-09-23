package server

import (
	"encoding/json"
	"sync/atomic"
)

// Stats is a point-in-time JSON representation of cache activity.
type Stats struct {
	Requests                   uint64 `json:"requests"`
	Operations                 uint64 `json:"operations"`
	Hits                       uint64 `json:"hits"`
	Misses                     uint64 `json:"misses"`
	Uploads                    uint64 `json:"uploads"`
	DeduplicatedUploads        uint64 `json:"deduplicated_uploads"`
	DiscardedUploads           uint64 `json:"discarded_uploads"`
	BackendRequests            uint64 `json:"backend_requests"`
	BackendDownloads           uint64 `json:"backend_downloads"`
	BackendExistenceChecks     uint64 `json:"backend_existence_checks"`
	BackendLoadErrors          uint64 `json:"backend_load_errors"`
	BackendSaveErrors          uint64 `json:"backend_save_errors"`
	RejectedRequests           uint64 `json:"rejected_requests"`
	ValidatedActionResults     uint64 `json:"validated_action_results"`
	IncompleteActionResults    uint64 `json:"incomplete_action_results"`
	InvalidActionResults       uint64 `json:"invalid_action_results"`
	SkippedActionResultUploads uint64 `json:"skipped_action_result_uploads"`
	BytesServed                uint64 `json:"bytes_served"`
	BytesReceived              uint64 `json:"bytes_received"`
	AssetRequests              uint64 `json:"asset_requests"`
	AssetHits                  uint64 `json:"asset_hits"`
	AssetDownloads             uint64 `json:"asset_downloads"`
	AssetRejected              uint64 `json:"asset_rejected"`
	AssetFetchErrors           uint64 `json:"asset_fetch_errors"`
	ThrottleWaits              uint64 `json:"throttle_waits"`
	PackUploads                uint64 `json:"pack_uploads"`
	ManifestUploads            uint64 `json:"manifest_uploads"`
	PackDownloads              uint64 `json:"pack_downloads"`
	PackBytesRestored          uint64 `json:"pack_bytes_restored"`
	PackBytesDeclared          uint64 `json:"pack_bytes_declared"`
	PackBytesUsed              uint64 `json:"pack_bytes_used"`
	PackLoadsSkipped           uint64 `json:"pack_loads_skipped"`
	PackRenewals               uint64 `json:"pack_renewals"`
	CompressedBlocks           uint64 `json:"compressed_blocks"`
	CompressionSavedBytes      uint64 `json:"compression_saved_bytes"`
	PacksDiscovered            uint64 `json:"packs_discovered"`
	ManifestsDiscovered        uint64 `json:"manifests_discovered"`
	ManifestsSkipped           uint64 `json:"manifests_skipped"`
	ManifestsOrphaned          uint64 `json:"manifests_orphaned"`
	ManifestDiscoveryErrors    uint64 `json:"manifest_discovery_errors"`
	ManifestLoadErrors         uint64 `json:"manifest_load_errors"`
	ActionDigestConflicts      uint64 `json:"action_digest_conflicts"`
}

type counters struct {
	requests                   atomic.Uint64
	operations                 atomic.Uint64
	hits                       atomic.Uint64
	misses                     atomic.Uint64
	uploads                    atomic.Uint64
	deduplicatedUploads        atomic.Uint64
	discardedUploads           atomic.Uint64
	backendRequests            atomic.Uint64
	backendDownloads           atomic.Uint64
	backendExistenceChecks     atomic.Uint64
	backendLoadErrors          atomic.Uint64
	backendSaveErrors          atomic.Uint64
	rejectedRequests           atomic.Uint64
	validatedActionResults     atomic.Uint64
	incompleteActionResults    atomic.Uint64
	invalidActionResults       atomic.Uint64
	skippedActionResultUploads atomic.Uint64
	bytesServed                atomic.Uint64
	bytesReceived              atomic.Uint64
	assetRequests              atomic.Uint64
	assetHits                  atomic.Uint64
	assetDownloads             atomic.Uint64
	assetRejected              atomic.Uint64
	assetFetchErrors           atomic.Uint64
	throttleWaits              atomic.Uint64
	packUploads                atomic.Uint64
	manifestUploads            atomic.Uint64
	packDownloads              atomic.Uint64
	packLoadsSkipped           atomic.Uint64
	packRenewals               atomic.Uint64
	compressedBlocks           atomic.Uint64
	compressionSavedBytes      atomic.Uint64
	packsDiscovered            atomic.Uint64
	manifestsDiscovered        atomic.Uint64
	manifestsSkipped           atomic.Uint64
	manifestsOrphaned          atomic.Uint64
	manifestDiscoveryErrors    atomic.Uint64
	manifestLoadErrors         atomic.Uint64
	actionDigestConflicts      atomic.Uint64
}

func (c *counters) snapshot() Stats {
	return Stats{
		Requests:                   c.requests.Load(),
		Operations:                 c.operations.Load(),
		Hits:                       c.hits.Load(),
		Misses:                     c.misses.Load(),
		Uploads:                    c.uploads.Load(),
		DeduplicatedUploads:        c.deduplicatedUploads.Load(),
		DiscardedUploads:           c.discardedUploads.Load(),
		BackendRequests:            c.backendRequests.Load(),
		BackendDownloads:           c.backendDownloads.Load(),
		BackendExistenceChecks:     c.backendExistenceChecks.Load(),
		BackendLoadErrors:          c.backendLoadErrors.Load(),
		BackendSaveErrors:          c.backendSaveErrors.Load(),
		RejectedRequests:           c.rejectedRequests.Load(),
		ValidatedActionResults:     c.validatedActionResults.Load(),
		IncompleteActionResults:    c.incompleteActionResults.Load(),
		InvalidActionResults:       c.invalidActionResults.Load(),
		SkippedActionResultUploads: c.skippedActionResultUploads.Load(),
		BytesServed:                c.bytesServed.Load(),
		BytesReceived:              c.bytesReceived.Load(),
		AssetRequests:              c.assetRequests.Load(),
		AssetHits:                  c.assetHits.Load(),
		AssetDownloads:             c.assetDownloads.Load(),
		AssetRejected:              c.assetRejected.Load(),
		AssetFetchErrors:           c.assetFetchErrors.Load(),
		ThrottleWaits:              c.throttleWaits.Load(),
		PackUploads:                c.packUploads.Load(),
		ManifestUploads:            c.manifestUploads.Load(),
		PackDownloads:              c.packDownloads.Load(),
		PackLoadsSkipped:           c.packLoadsSkipped.Load(),
		PackRenewals:               c.packRenewals.Load(),
		CompressedBlocks:           c.compressedBlocks.Load(),
		CompressionSavedBytes:      c.compressionSavedBytes.Load(),
		PacksDiscovered:            c.packsDiscovered.Load(),
		ManifestsDiscovered:        c.manifestsDiscovered.Load(),
		ManifestsSkipped:           c.manifestsSkipped.Load(),
		ManifestsOrphaned:          c.manifestsOrphaned.Load(),
		ManifestDiscoveryErrors:    c.manifestDiscoveryErrors.Load(),
		ManifestLoadErrors:         c.manifestLoadErrors.Load(),
		ActionDigestConflicts:      c.actionDigestConflicts.Load(),
	}
}

func (s Stats) JSON() []byte {
	data, _ := json.Marshal(s)
	return data
}
