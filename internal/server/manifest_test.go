package server

import (
	"bytes"
	"testing"
)

func TestManifestDAGCBORRoundTripIsDeterministic(t *testing.T) {
	casDigest := digest([]byte("cas"))
	casCID, err := rawCIDForDigest(casDigest)
	if err != nil {
		t.Fatal(err)
	}
	actionCID, err := rawCIDForData([]byte{0x08, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	packID := digest([]byte("pack"))
	compressedCID, err := rawCIDForData([]byte("compressed action bytes"))
	if err != nil {
		t.Fatal(err)
	}
	value := manifest{
		Version: manifestFormatVersion,
		Parents: []string{},
		Packs:   []packDescriptor{{ID: packID, Key: packKeyFor("test", packID), Size: 123}},
		CAS: []manifestObject{{
			Digest: casDigest,
			CID:    casCID.String(),
			Block:  casCID.String(),
			PackID: packID,
			Size:   3,
		}},
		Actions: []manifestAction{{
			Digest:   digest([]byte("action")),
			CID:      actionCID.String(),
			Block:    compressedCID.String(),
			Encoding: blockEncodingZstd,
			PackID:   packID,
			Size:     2,
		}},
	}
	encoded, manifestCID, err := encodeManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeManifest(encoded, manifestCID)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, secondCID, err := encodeManifest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if manifestCID != secondCID || !bytes.Equal(encoded, reencoded) {
		t.Fatal("DAG-CBOR manifest is not deterministic")
	}
}

func TestManifestFormatTwoOmitsRawBlockMetadataAndClosureValues(t *testing.T) {
	packID := digest([]byte("pack"))
	casData := []byte("raw cas")
	casDigest := digest(casData)
	casCID, err := rawCIDForDigest(casDigest)
	if err != nil {
		t.Fatal(err)
	}
	actionData := []byte{0x08, 0x01}
	actionCID, err := rawCIDForData(actionData)
	if err != nil {
		t.Fatal(err)
	}
	value := manifest{
		Version: manifestFormatVersion,
		Packs:   []packDescriptor{{ID: packID, Key: packKeyFor("test", packID), Size: 128}},
		CAS: []manifestObject{{
			Digest: casDigest, CID: casCID.String(), Block: casCID.String(),
			PackID: packID, Size: int64(len(casData)),
		}},
		Actions: []manifestAction{{
			Digest: digest(actionData), CID: actionCID.String(), Block: actionCID.String(),
			PackID: packID, Size: int64(len(actionData)),
		}},
	}
	node, err := manifestToNode(value)
	if err != nil {
		t.Fatal(err)
	}

	casList, err := node.LookupByString("cas")
	if err != nil {
		t.Fatal(err)
	}
	_, casEntry, err := casList.ListIterator().Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := casEntry.LookupByString("block"); err == nil {
		t.Fatal("raw CAS entry contains redundant block field")
	}
	if _, err := casEntry.LookupByString("encoding"); err == nil {
		t.Fatal("raw CAS entry contains redundant encoding field")
	}
	casBlock, casEncoding, err := nodeBlockEncoding(casEntry, casCID.String())
	if err != nil || casBlock != casCID.String() || casEncoding != blockEncodingRaw {
		t.Fatalf("omitted raw CAS fields decode as %q/%q, err %v", casBlock, casEncoding, err)
	}

	actionList, err := node.LookupByString("actions")
	if err != nil {
		t.Fatal(err)
	}
	_, actionEntry, err := actionList.ListIterator().Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := actionEntry.LookupByString("block"); err == nil {
		t.Fatal("raw action entry contains redundant block field")
	}
	if _, err := actionEntry.LookupByString("encoding"); err == nil {
		t.Fatal("raw action entry contains redundant encoding field")
	}
	closure, err := nodeStringList(actionEntry, "closure")
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != 0 {
		t.Fatalf("new action manifest carries unused closure IDs: %v", closure)
	}
}

func TestManifestRejectsTamperedCID(t *testing.T) {
	value := manifest{Version: manifestFormatVersion}
	encoded, manifestCID, err := encodeManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := decodeManifest(encoded, manifestCID); err == nil {
		t.Fatal("tampered manifest was accepted")
	}
}

// A pack of many small objects yields a manifest past the DAG-CBOR decoder's
// default allocation budget; it must still round-trip.
func TestManifestWithManyEntriesDecodes(t *testing.T) {
	packID := digest([]byte("pack"))
	value := manifest{
		Version: manifestFormatVersion,
		Parents: []string{},
		Packs:   []packDescriptor{{ID: packID, Key: packKeyFor("test", packID), Size: 1}},
	}
	for i := 0; i < 60000; i++ {
		data := []byte{byte(i), byte(i >> 8), byte(i >> 16)}
		casCID, err := rawCIDForData(data)
		if err != nil {
			t.Fatal(err)
		}
		value.CAS = append(value.CAS, manifestObject{
			Digest: digest(data),
			CID:    casCID.String(),
			Block:  casCID.String(),
			PackID: packID,
			Size:   int64(len(data)),
		})
	}
	encoded, manifestCID, err := encodeManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeManifest(encoded, manifestCID)
	if err != nil {
		t.Fatalf("decode %d-byte manifest: %v", len(encoded), err)
	}
	if len(decoded.CAS) != len(value.CAS) {
		t.Fatalf("decoded %d CAS entries, want %d", len(decoded.CAS), len(value.CAS))
	}
}

// A pack's age is optional: manifests without it still decode, and one that
// records it survives a round trip unchanged.
func TestManifestCarriesAnOptionalPackAge(t *testing.T) {
	packID := digest([]byte("pack"))
	for _, since := range []int64{0, 1_790_000_000} {
		value := manifest{
			Version: manifestFormatVersion,
			Parents: []string{},
			Packs:   []packDescriptor{{ID: packID, Key: packKeyFor("test", packID), Size: 1, Since: since}},
		}
		encoded, manifestCID, err := encodeManifest(value)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeManifest(encoded, manifestCID)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Packs[0].Since != since {
			t.Fatalf("since = %d, want %d", decoded.Packs[0].Since, since)
		}
		reencoded, _, err := encodeManifest(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encoded, reencoded) {
			t.Fatal("a manifest with a pack age is not deterministic")
		}
	}
}
