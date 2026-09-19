package server

import (
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Block encodings recorded in the manifest. An empty encoding means the block
// holds the object's bytes verbatim, which keeps older manifests readable.
const (
	blockEncodingRaw  = ""
	blockEncodingZstd = "zstd"
)

const (
	// Below this size a frame header costs more than compression saves.
	minCompressibleBlockSize = 256
	// Compressed output must be at least this much smaller to be worth the
	// decompression on every later read.
	maxUsefulCompressionRatio = 0.95
	// Large objects are sampled first so an incompressible one, such as a
	// gzipped OCI layer, is rejected after a few hundred KB rather than after
	// compressing hundreds of MB.
	compressionProbeSize      = 256 << 10
	compressionProbeThreshold = 4 * compressionProbeSize
)

// blockCodec compresses individual pack blocks. Compressing each block on its
// own keeps the CARv2 index meaningful: a reader restores one block and
// decompresses only that block, instead of expanding a whole pack to serve one
// object.
type blockCodec struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
	enabled bool
}

func newBlockCodec(enabled bool, level, maxBlobSize int64) (*blockCodec, error) {
	// Both are safe for concurrent EncodeAll/DecodeAll use.
	encoder, err := zstd.NewWriter(
		nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(int(level))),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	decoder, err := zstd.NewReader(
		nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(maxBlobSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	return &blockCodec{encoder: encoder, decoder: decoder, enabled: enabled}, nil
}

// encode returns the bytes to store and the encoding to record. It falls back
// to the original bytes whenever compression does not pay for itself.
func (c *blockCodec) encode(data []byte) ([]byte, string) {
	if !c.enabled || len(data) < minCompressibleBlockSize {
		return data, blockEncodingRaw
	}
	if len(data) > compressionProbeThreshold && !c.probeWorthwhile(data) {
		return data, blockEncodingRaw
	}
	compressed := c.encoder.EncodeAll(data, nil)
	if float64(len(compressed)) > float64(len(data))*maxUsefulCompressionRatio {
		return data, blockEncodingRaw
	}
	return compressed, blockEncodingZstd
}

func (c *blockCodec) probeWorthwhile(data []byte) bool {
	sample := data[:compressionProbeSize]
	compressed := c.encoder.EncodeAll(sample, nil)
	return float64(len(compressed)) <= float64(len(sample))*maxUsefulCompressionRatio
}

// decode restores a stored block. plainSize comes from the manifest and bounds
// the output, so a corrupt or hostile frame cannot expand without limit.
func (c *blockCodec) decode(stored []byte, encoding string, plainSize int64) ([]byte, error) {
	switch encoding {
	case blockEncodingRaw:
		return stored, nil
	case blockEncodingZstd:
		if plainSize < 0 {
			return nil, errors.New("compressed block has no declared size")
		}
		plain, err := c.decoder.DecodeAll(stored, make([]byte, 0, plainSize))
		if err != nil {
			return nil, fmt.Errorf("decompress block: %w", err)
		}
		if int64(len(plain)) != plainSize {
			return nil, fmt.Errorf("decompressed block is %d bytes; manifest declares %d", len(plain), plainSize)
		}
		return plain, nil
	default:
		return nil, fmt.Errorf("unsupported block encoding %q", encoding)
	}
}

func (c *blockCodec) close() {
	_ = c.encoder.Close()
	c.decoder.Close()
}

func validBlockEncoding(encoding string) bool {
	return encoding == blockEncodingRaw || encoding == blockEncodingZstd
}
