package store

import (
	"encoding/json"
	"io"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
)

// ChunkThreshold is the size above which file bytes are stored as a
// chunked_blob instead of one blob.
const ChunkThreshold = 4 << 20

const (
	chunkMin = 16 << 10
	chunkAvg = 64 << 10
	chunkMax = 256 << 10
)

type chunkList struct {
	Chunks []string `json:"chunks"`
	Size   int64    `json:"size"`
}

// PutFile stores file bytes and returns the id that tree entries name: a
// blob, or a chunked_blob for files over ChunkThreshold.
func (b *Batch) PutFile(data []byte) (string, error) {
	if len(data) <= ChunkThreshold {
		return b.Put(KindBlob, data)
	}
	list := chunkList{Size: int64(len(data))}
	for _, c := range chunks(data) {
		id, err := b.Put(KindBlob, c)
		if err != nil {
			return "", err
		}
		list.Chunks = append(list.Chunks, id)
	}
	raw, err := canon.Marshal(list)
	if err != nil {
		return "", err
	}
	return b.Put(KindChunkedBlob, raw)
}

// FileID returns the id PutFile would give data, without storing it.
func FileID(data []byte) (string, error) {
	if len(data) <= ChunkThreshold {
		return ID(KindBlob, data), nil
	}
	list := chunkList{Size: int64(len(data))}
	for _, c := range chunks(data) {
		list.Chunks = append(list.Chunks, ID(KindBlob, c))
	}
	raw, err := canon.Marshal(list)
	if err != nil {
		return "", err
	}
	return ID(KindChunkedBlob, raw), nil
}

// WriteFile streams the bytes of a blob or chunked_blob to w.
func (s *Store) WriteFile(id string, w io.Writer) error {
	kind, data, err := s.Get(id)
	if err != nil {
		return err
	}
	switch kind {
	case KindBlob:
		_, err = w.Write(data)
		return err
	case KindChunkedBlob:
		var list chunkList
		if err := json.Unmarshal(data, &list); err != nil {
			return apierr.New("object_corrupt", "chunk list %s is malformed", id).With("object", id)
		}
		var n int64
		for _, c := range list.Chunks {
			chunk, err := s.getKind(c, KindBlob)
			if err != nil {
				return err
			}
			if _, err := w.Write(chunk); err != nil {
				return err
			}
			n += int64(len(chunk))
		}
		if n != list.Size {
			return apierr.New("object_corrupt", "chunk list %s has %d bytes, not %d", id, n, list.Size).With("object", id)
		}
		return nil
	}
	return apierr.New("object_kind", "object %s is a %s, not file bytes", id, kind).With("object", id)
}

// gear comes from SplitMix64 with a fixed seed, written out here rather
// than taken from math/rand, so chunk boundaries and chunked_blob ids can
// never change with a Go release.
var gear = func() [256]uint64 {
	var g [256]uint64
	x := uint64(0x6c65646765720a32) // "ledger\n2"
	for i := range g {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
		z = (z ^ z>>27) * 0x94d049bb133111eb
		g[i] = z ^ z>>31
	}
	return g
}()

// chunks cuts data with FastCDC-style gear hashing and normalized
// chunking: a harder mask before the average size, an easier one after.
func chunks(data []byte) [][]byte {
	const maskS, maskL = (1<<18 - 1) << 46, (1<<14 - 1) << 50
	var out [][]byte
	for len(data) > 0 {
		n := min(len(data), chunkMax)
		cut := n
		if n > chunkMin {
			var h uint64
			for i := chunkMin; i < n; i++ {
				h = h<<1 + gear[data[i]]
				mask := uint64(maskS)
				if i >= chunkAvg {
					mask = maskL
				}
				if h&mask == 0 {
					cut = i + 1
					break
				}
			}
		}
		out = append(out, data[:cut])
		data = data[cut:]
	}
	return out
}
