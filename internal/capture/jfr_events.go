package capture

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// A minimal reader for the JFR file format (version 2, JDK 8u262 and later),
// enough to count the events of given types without a JDK. It follows the
// JDK's own reader (jdk.jfr.internal.consumer.ChunkHeader, ChunkParser and
// jdk.jfr.internal.MetadataReader).
//
// A file is a sequence of chunks. Each chunk is a 68-byte header followed by
// records: a varint size, a varint type id, then the payload. Type 0 is the
// metadata record, which maps type ids to class names; type 1 is a constant
// pool. Every other type id is an event.

const (
	jfrChunkHeaderSize = 68
	jfrMetadataTypeID  = 0
	jfrFlagCompressed  = 1 // flag byte bit: integers are varints
	jfrMaxElementDepth = 64
)

var jfrMagic = []byte("FLR\x00")

// jfrEventCounts counts the events in the .jfr file p whose type is one of
// names.
func jfrEventCounts(p string, names []string) (map[string]int, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return jfrCountEvents(f, info.Size(), names)
}

func jfrCountEvents(r io.ReaderAt, size int64, names []string) (map[string]int, error) {
	counts := make(map[string]int, len(names))
	for _, n := range names {
		counts[n] = 0
	}
	if size == 0 {
		return nil, errors.New("the file is empty")
	}
	for start, chunk := int64(0), 1; start < size; chunk++ {
		chunkSize, err := jfrCountChunk(r, start, size-start, names, counts)
		if err != nil {
			return nil, fmt.Errorf("chunk %d at byte %d: %w", chunk, start, err)
		}
		start += chunkSize
	}
	return counts, nil
}

// jfrCountChunk adds the events of the chunk at start to counts and returns
// the chunk's size.
func jfrCountChunk(r io.ReaderAt, start, avail int64, names []string, counts map[string]int) (int64, error) {
	var h [jfrChunkHeaderSize]byte
	if avail < jfrChunkHeaderSize {
		return 0, errors.New("incomplete chunk header")
	}
	if _, err := r.ReadAt(h[:], start); err != nil {
		return 0, fmt.Errorf("reading the chunk header: %w", err)
	}
	if string(h[0:4]) != string(jfrMagic) {
		return 0, errors.New("not a JFR file")
	}
	if major := binary.BigEndian.Uint16(h[4:6]); major != 2 {
		return 0, fmt.Errorf("JFR file version %d.%d, but only version 2 can be read", major, binary.BigEndian.Uint16(h[6:8]))
	}
	chunkSize := int64(binary.BigEndian.Uint64(h[8:16]))
	metadataPos := int64(binary.BigEndian.Uint64(h[24:32]))
	switch {
	case chunkSize < jfrChunkHeaderSize || chunkSize > avail:
		return 0, fmt.Errorf("chunk size %d doesn't fit the %d bytes left in the file", chunkSize, avail)
	case h[64] != 0:
		return 0, errors.New("the chunk is not finished")
	case h[67]&jfrFlagCompressed == 0:
		return 0, errors.New("the chunk doesn't use compressed integers")
	case metadataPos < jfrChunkHeaderSize || metadataPos >= chunkSize:
		return 0, fmt.Errorf("metadata position %d is outside the chunk", metadataPos)
	}

	ids, err := jfrTypeIDs(io.NewSectionReader(r, start+metadataPos, chunkSize-metadataPos), names)
	if err != nil {
		return 0, fmt.Errorf("reading the metadata: %w", err)
	}

	rd := &jfrInput{r: bufio.NewReaderSize(io.NewSectionReader(r, start+jfrChunkHeaderSize, chunkSize-jfrChunkHeaderSize), 64<<10)}
	for left := chunkSize - jfrChunkHeaderSize; left > 0; {
		rd.n = 0
		recSize, err := rd.varint()
		if err != nil {
			return 0, fmt.Errorf("reading a record size: %w", err)
		}
		typeID, err := rd.varint()
		if err != nil {
			return 0, fmt.Errorf("reading a record type: %w", err)
		}
		if recSize < rd.n || recSize > left {
			return 0, fmt.Errorf("record size %d doesn't fit the %d bytes left in the chunk", recSize, left)
		}
		if name, ok := ids[typeID]; ok {
			counts[name]++
		}
		if _, err := rd.r.Discard(int(recSize - rd.n)); err != nil {
			return 0, fmt.Errorf("skipping a record: %w", err)
		}
		left -= recSize
	}
	return chunkSize, nil
}

// jfrTypeIDs reads a metadata record and returns the type id of each class
// in names that it declares.
func jfrTypeIDs(sr *io.SectionReader, names []string) (map[int64]string, error) {
	rd := &jfrInput{r: bufio.NewReader(sr)}
	limit := sr.Size()
	if _, err := rd.varint(); err != nil { // record size
		return nil, err
	}
	typeID, err := rd.varint()
	if err != nil {
		return nil, err
	}
	if typeID != jfrMetadataTypeID {
		return nil, fmt.Errorf("type id %d, want the metadata type %d", typeID, jfrMetadataTypeID)
	}
	for range 3 { // start time, duration, metadata id
		if _, err := rd.varint(); err != nil {
			return nil, err
		}
	}

	poolSize, err := rd.count(limit)
	if err != nil {
		return nil, fmt.Errorf("string pool size: %w", err)
	}
	pool := make([]string, poolSize)
	for i := range pool {
		if pool[i], err = rd.string(limit); err != nil {
			return nil, fmt.Errorf("string %d: %w", i, err)
		}
	}

	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}
	ids := map[int64]string{}
	var element func(depth int) error
	element = func(depth int) error {
		if depth > jfrMaxElementDepth {
			return errors.New("elements nested too deeply")
		}
		name, err := rd.poolString(pool)
		if err != nil {
			return err
		}
		attrs, err := rd.count(limit)
		if err != nil {
			return err
		}
		var className, id string
		for range attrs {
			k, err := rd.poolString(pool)
			if err != nil {
				return err
			}
			v, err := rd.poolString(pool)
			if err != nil {
				return err
			}
			switch k {
			case "name":
				className = v
			case "id":
				id = v
			}
		}
		if name == "class" && wanted[className] {
			n, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				return fmt.Errorf("class %s has id %q", className, id)
			}
			ids[n] = className
		}
		children, err := rd.count(limit)
		if err != nil {
			return err
		}
		for range children {
			if err := element(depth + 1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := element(0); err != nil {
		return nil, err
	}
	return ids, nil
}

// jfrInput reads JFR's encodings and counts the bytes it consumes.
type jfrInput struct {
	r *bufio.Reader
	n int64
}

func (in *jfrInput) byte() (byte, error) {
	b, err := in.r.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	in.n++
	return b, nil
}

// varint reads JFR's compressed integer: 7 bits per byte, low bits first,
// with the ninth byte holding a full 8 bits.
func (in *jfrInput) varint() (int64, error) {
	var v uint64
	for i := range 8 {
		b, err := in.byte()
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return int64(v), nil
		}
	}
	b, err := in.byte()
	if err != nil {
		return 0, err
	}
	return int64(v | uint64(b)<<56), nil
}

// count reads a varint that counts items, each taking at least a byte of
// the limit bytes available.
func (in *jfrInput) count(limit int64) (int, error) {
	v, err := in.varint()
	if err != nil {
		return 0, err
	}
	if v < 0 || v > limit {
		return 0, fmt.Errorf("count %d is out of range", v)
	}
	return int(v), nil
}

func (in *jfrInput) poolString(pool []string) (string, error) {
	i, err := in.varint()
	if err != nil {
		return "", err
	}
	if i < 0 || i >= int64(len(pool)) {
		return "", fmt.Errorf("string index %d is outside the string pool", i)
	}
	return pool[i], nil
}

// string reads a string as JFR encodes it: an encoding byte, then for the
// array encodings a length and the bytes or chars.
func (in *jfrInput) string(limit int64) (string, error) {
	enc, err := in.byte()
	if err != nil {
		return "", err
	}
	switch enc {
	case 0, 1: // null, empty
		return "", nil
	case 3, 5: // UTF-8, Latin-1
		n, err := in.count(limit)
		if err != nil {
			return "", err
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(in.r, b); err != nil {
			return "", err
		}
		in.n += int64(n)
		if enc == 3 {
			return string(b), nil
		}
		runes := make([]rune, n)
		for i, c := range b {
			runes[i] = rune(c)
		}
		return string(runes), nil
	case 4: // chars, each a varint
		n, err := in.count(limit)
		if err != nil {
			return "", err
		}
		runes := make([]rune, n)
		for i := range runes {
			c, err := in.varint()
			if err != nil {
				return "", err
			}
			runes[i] = rune(c)
		}
		return string(runes), nil
	default:
		return "", fmt.Errorf("unknown string encoding %d", enc)
	}
}
