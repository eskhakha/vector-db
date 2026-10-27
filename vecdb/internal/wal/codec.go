package wal

import (
	"encoding/binary"
	"hash/crc32"
)

// EncodeFrame returns the full on-disk frame for a record: header plus payload.
//
// It is exported so that snapshots can reuse the exact same codec as the log.
// One codec means one set of framing bugs, and it means a snapshot and a log
// record for the same vector are byte-identical in their payload -- which makes
// "did recovery produce the same state" a checkable question.
func EncodeFrame(r *Record) []byte {
	payload := encodePayload(r)
	buf := make([]byte, headerSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:], magic)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(payload)))
	binary.LittleEndian.PutUint64(buf[12:], uint64(r.LSN))
	copy(buf[headerSize:], payload)
	binary.LittleEndian.PutUint32(buf[8:], crc32.Checksum(buf[12:], castagnoli))
	return buf
}

// DecodeFrame parses one frame at off. ok=false means there is no valid record
// at that offset, which covers both a clean end of data and a torn tail.
func DecodeFrame(data []byte, off int) (rec *Record, next int, ok bool) {
	return decodeAt(data, off)
}
