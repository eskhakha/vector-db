package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/eskhakha/vecdb/internal/core"
)

// OpType is the operation a log record describes.
type OpType uint8

const (
	OpPut OpType = 1
	OpDel OpType = 2
)

// Record is one entry in the log.
type Record struct {
	LSN   core.LSN
	Epoch core.Epoch
	Op    OpType
	// Vec carries the full record for OpPut. For OpDel only ID is meaningful.
	Vec core.Vector
}

// Payload framing.
//
// The frame is deliberately hand-rolled and self-describing. A log reader's
// input is not a trusted message from a peer -- it is a file that may have been
// torn in half by a crash -- so every field it reads must be validated before
// it is used to size an allocation or advance a cursor.
//
//	0        4        8       12                20             20+len
//	+--------+--------+--------+----------------+------------------+
//	| MAGIC  | LEN    | CRC32C | LSN            | PAYLOAD          |
//	| 4B     | 4B u32 | 4B     | 8B u64 LE      | LEN bytes        |
//	+--------+--------+--------+----------------+------------------+
//
// CRC covers LSN || PAYLOAD, not the header: MAGIC and LEN must be validated
// structurally *before* we are willing to read LEN bytes, and the CRC then
// validates the content we were told to read.
const (
	headerSize = 20
	magic      = uint32(0x56454331) // "VEC1"
	// maxRecordSize bounds a length prefix so a garbage LEN from a torn write
	// can never cause a huge allocation.
	maxRecordSize = 4 << 20
)

var errBadFrame = errors.New("wal: bad frame")

// encodePayload serializes a record body. Layout:
//
//	epoch u64 | op u8 | idLen u16 | id | deleted u8 | version u64 |
//	createdAt i64 | updatedAt i64 | norm f32 | dim u32 | dim*f32 |
//	metaCount u16 | (kLen u16 | k | vLen u32 | v)*
func encodePayload(r *Record) []byte {
	v := &r.Vec
	size := 8 + 1 + 2 + len(v.ID) + 1 + 8 + 8 + 8 + 4 + 4 + 4*len(v.Embedding) + 2
	for k, val := range v.Metadata {
		size += 2 + len(k) + 4 + len(val)
	}
	b := make([]byte, 0, size)
	b = binary.LittleEndian.AppendUint64(b, uint64(r.Epoch))
	b = append(b, byte(r.Op))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(v.ID)))
	b = append(b, v.ID...)
	var del byte
	if v.Deleted {
		del = 1
	}
	b = append(b, del)
	b = binary.LittleEndian.AppendUint64(b, uint64(v.Version))
	b = binary.LittleEndian.AppendUint64(b, uint64(v.CreatedAt))
	b = binary.LittleEndian.AppendUint64(b, uint64(v.UpdatedAt))
	b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v.Norm))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(v.Embedding)))
	for _, f := range v.Embedding {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(f))
	}
	b = binary.LittleEndian.AppendUint16(b, uint16(len(v.Metadata)))
	// Metadata keys are sorted before encoding. Go randomizes map iteration
	// order, so without this the same logical record encodes to different bytes
	// on every call -- which would mean two replicas applying the same write
	// produce different log bytes, making a byte-level log comparison between
	// replicas impossible. Determinism here is a correctness property, not a
	// tidiness preference.
	for _, k := range sortedKeys(v.Metadata) {
		val := v.Metadata[k]
		b = binary.LittleEndian.AppendUint16(b, uint16(len(k)))
		b = append(b, k...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(val)))
		b = append(b, val...)
	}
	return b
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// cursor is a bounds-checked reader over a payload. Every accessor returns an
// error rather than panicking, because the bytes may be arbitrary garbage.
type cursor struct {
	b   []byte
	i   int
	err error
}

func (c *cursor) need(n int) bool {
	if c.err != nil {
		return false
	}
	if c.i+n > len(c.b) {
		c.err = fmt.Errorf("%w: want %d bytes at %d, have %d", errBadFrame, n, c.i, len(c.b))
		return false
	}
	return true
}

func (c *cursor) u8() byte {
	if !c.need(1) {
		return 0
	}
	v := c.b[c.i]
	c.i++
	return v
}

func (c *cursor) u16() uint16 {
	if !c.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(c.b[c.i:])
	c.i += 2
	return v
}

func (c *cursor) u32() uint32 {
	if !c.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(c.b[c.i:])
	c.i += 4
	return v
}

func (c *cursor) u64() uint64 {
	if !c.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(c.b[c.i:])
	c.i += 8
	return v
}

func (c *cursor) bytes(n int) []byte {
	if n < 0 || !c.need(n) {
		return nil
	}
	v := c.b[c.i : c.i+n]
	c.i += n
	return v
}

func decodePayload(lsn core.LSN, p []byte) (*Record, error) {
	c := &cursor{b: p}
	r := &Record{LSN: lsn}
	r.Epoch = core.Epoch(c.u64())
	r.Op = OpType(c.u8())
	idLen := int(c.u16())
	if idLen > core.MaxIDLen {
		return nil, fmt.Errorf("%w: id length %d", errBadFrame, idLen)
	}
	r.Vec.ID = string(c.bytes(idLen))
	r.Vec.Deleted = c.u8() == 1
	r.Vec.Version = core.LSN(c.u64())
	r.Vec.CreatedAt = int64(c.u64())
	r.Vec.UpdatedAt = int64(c.u64())
	r.Vec.Norm = math.Float32frombits(c.u32())
	dim := int(c.u32())
	// Guard the allocation: 4 bytes per element must still be inside the frame.
	if dim < 0 || dim > (len(p)-c.i)/4 {
		return nil, fmt.Errorf("%w: dim %d", errBadFrame, dim)
	}
	if dim > 0 {
		r.Vec.Embedding = make([]float32, dim)
		for i := range r.Vec.Embedding {
			r.Vec.Embedding[i] = math.Float32frombits(c.u32())
		}
	}
	n := int(c.u16())
	if n > 0 {
		r.Vec.Metadata = make(map[string]string, n)
		for i := 0; i < n; i++ {
			k := string(c.bytes(int(c.u16())))
			v := string(c.bytes(int(c.u32())))
			if c.err != nil {
				break
			}
			r.Vec.Metadata[k] = v
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	if r.Op != OpPut && r.Op != OpDel {
		return nil, fmt.Errorf("%w: op %d", errBadFrame, r.Op)
	}
	return r, nil
}
