// Package pst reads Microsoft Personal Folder Files (PST) and Offline Storage
// Tables (OST) well enough to import the mail, calendar and contact items they
// hold.
//
// The layout is the one described by [MS-PST]: a 512 byte header, two B-trees
// (the offsets index, which maps a data identifier to the bytes in the file,
// and the descriptors index, which maps an item identifier to its data, its
// local descriptors and its parent), and tables of record entries carrying the
// MAPI properties of every item.
//
// This is a from-scratch reader. It handles the 32-bit (ANSI, Outlook 97-2002)
// and 64-bit (Unicode, Outlook 2007+) layouts, the per-block obfuscation, and
// the data-array indirection used for items larger than a single block. The
// 4k-page compressed OST layout is rejected with a clear message rather than
// half-parsed.
package pst

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	headerSize = 512
	nodeSize   = 512

	fileSignature = "!BDN"

	nodeTypeFolder            = 0x02
	nodeTypeSearchFolder      = 0x03
	nodeTypeMessage           = 0x04
	nodeTypeAttachment        = 0x05
	nodeTypeAssociatedContent = 0x08
	nodeTypeSubFolders        = 0x0d
	nodeTypeSubMessages       = 0x0e
	nodeTypeAttachments       = 0x11
	nodeTypeRecipients        = 0x12
	nodeTypeLocalDescriptor   = 0x1f

	// The local descriptor identifiers every message item carries: the
	// attachment table and the recipient table.
	attachmentsTableIdentifier = 0x0671
	recipientsTableIdentifier  = 0x0692

	// A data identifier with this bit set is an array of data identifiers
	// rather than data itself.
	flagInternal = 1 << 1
)

// ErrUnsupported reports a file this reader cannot open: not a PST at all, or
// a PST variant whose parsing is a separate project.
type ErrUnsupported struct {
	Reason string
}

func (e ErrUnsupported) Error() string { return "pst: " + e.Reason }

// RandomAccess is the slice of a file the reader needs. The browser reads a PST
// with File.slice; this interface keeps both sides shaped the same and lets a
// test hand in an in-memory buffer.
type RandomAccess interface {
	Size() (int64, error)
	ReadAt(offset, length int64) ([]byte, error)
}

// bufferAccess adapts a byte slice, which is what tests use.
type bufferAccess struct {
	data []byte
}

func (b *bufferAccess) Size() (int64, error) { return int64(len(b.data)), nil }

func (b *bufferAccess) ReadAt(offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, nil
	}
	if offset >= int64(len(b.data)) {
		return nil, fmt.Errorf("pst: read past the end of the file at %d", offset)
	}
	end := offset + length
	if end > int64(len(b.data)) {
		end = int64(len(b.data))
	}
	return b.data[offset:end], nil
}

// New opens a PST from any random access source.
func New(rand RandomAccess) (*File, error) {
	p := &File{
		rand:        rand,
		offsets:     map[uint64]offsetEntry{},
		descriptors: map[uint64]descriptorEntry{},
		blocks:      map[uint64][]block{},
	}
	if err := p.readHeader(); err != nil {
		return nil, err
	}
	if err := p.readIndexes(); err != nil {
		return nil, err
	}
	return p, nil
}

// Open reads a PST or OST from disk. The caller should Close the file.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pst: opening %s: %w", path, err)
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("pst: stat %s: %w", path, err)
	}
	file, err := New(&fileAccess{f: f, size: stat.Size()})
	if err != nil {
		f.Close()
		return nil, err
	}
	file.closer = f
	return file, nil
}

// Close releases the underlying file, when the reader opened one.
func (p *File) Close() error {
	if p.closer != nil {
		return p.closer.Close()
	}
	return nil
}

type fileAccess struct {
	f    *os.File
	size int64
}

func (a *fileAccess) Size() (int64, error) { return a.size, nil }

func (a *fileAccess) ReadAt(offset, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	buf := make([]byte, length)
	n, err := a.f.ReadAt(buf, offset)
	if err != nil && n < int(length) {
		return nil, fmt.Errorf("pst: reading at %d: %w", offset, err)
	}
	return buf, nil
}

// offsetEntry is one leaf of the offsets index: where a data block lives.
type offsetEntry struct {
	identifier uint64
	fileOffset uint64
	dataSize   uint32
	refCount   uint32
}

// descriptorEntry is one leaf of the descriptors index: one item node.
type descriptorEntry struct {
	identifier       uint64
	dataIdentifier   uint64
	localDescriptors uint64
	parentIdentifier uint32
}

// block is one table array entry: the decrypted bytes of a data block with its
// footer removed. Table indexes are relative to a block, so the block
// boundaries have to survive the read.
type block struct {
	data  []byte
	index []indexValue
}

type indexValue struct {
	start uint16
	end   uint16
}

// File is an opened PST.
type File struct {
	rand        RandomAccess
	closer      io.Closer
	is64        bool
	encType     uint8
	offsetsRoot uint64
	descsRoot   uint64

	offsets     map[uint64]offsetEntry
	descriptors map[uint64]descriptorEntry
	blocks      map[uint64][]block
	names       map[uint32]NamedProperty
}

func (p *File) readHeader() error {
	// The 64-bit header carries 52 bytes of trailing fields past the 512 byte
	// page, so the reader asks for a little more than a page.
	readSize := int64(564)
	if size, err := p.rand.Size(); err == nil && size < readSize {
		readSize = size
	}
	data, err := p.rand.ReadAt(0, readSize)
	if err != nil {
		return fmt.Errorf("pst: reading header: %w", err)
	}
	if len(data) < headerSize || string(data[:4]) != fileSignature {
		return ErrUnsupported{Reason: "not a Personal Folder File"}
	}

	dataVersion := binary.LittleEndian.Uint16(data[10:12])
	switch {
	case dataVersion <= 0x000f:
		p.is64 = false
	case dataVersion >= 0x0024:
		return ErrUnsupported{Reason: "compressed 4k page OST files are not supported; " +
			"open the file in Outlook and export it to a .pst first"}
	case dataVersion >= 0x0015:
		p.is64 = true
	default:
		// An unseen version: fall back to the sentinel byte, as other readers do.
		if data[460] == 0x80 {
			p.is64 = false
		} else if data[512] == 0x80 {
			p.is64 = true
		} else {
			return ErrUnsupported{Reason: fmt.Sprintf("unknown data version 0x%04x", dataVersion)}
		}
	}

	if p.is64 {
		p.encType = data[513]
		p.descsRoot = binary.LittleEndian.Uint64(data[224:232])
		p.offsetsRoot = binary.LittleEndian.Uint64(data[240:248])
	} else {
		p.encType = data[461]
		p.descsRoot = uint64(binary.LittleEndian.Uint32(data[188:192]))
		p.offsetsRoot = uint64(binary.LittleEndian.Uint32(data[196:200]))
	}

	if p.offsetsRoot == 0 || p.descsRoot == 0 {
		return ErrUnsupported{Reason: "the file header has no index root"}
	}
	return nil
}

// ContentType returns "PST", "OST" or "PAB".
func (p *File) ContentType() string {
	data, err := p.rand.ReadAt(0, 10)
	if err != nil || len(data) < 10 {
		return ""
	}
	switch string(data[8:10]) {
	case "SM", "MS":
		return "PST"
	case "SO", "OS":
		return "OST"
	default:
		return "PAB"
	}
}

// Is64Bit reports whether the file uses the 64-bit (Unicode) layout.
func (p *File) Is64Bit() bool { return p.is64 }

func (p *File) readIndexes() error {
	if err := p.walkIndex(p.offsetsRoot, true, 0); err != nil {
		return fmt.Errorf("pst: reading offsets index: %w", err)
	}
	if len(p.offsets) == 0 {
		return ErrUnsupported{Reason: "the offsets index is empty"}
	}
	if err := p.walkIndex(p.descsRoot, false, 0); err != nil {
		return fmt.Errorf("pst: reading descriptors index: %w", err)
	}
	if len(p.descriptors) == 0 {
		return ErrUnsupported{Reason: "the descriptors index is empty"}
	}
	return nil
}

// walkIndex visits every node of one index B-tree.
func (p *File) walkIndex(offset uint64, offsetIndex bool, depth int) error {
	if depth > 12 {
		return fmt.Errorf("pst: index tree deeper than 12 levels")
	}
	data, err := p.rand.ReadAt(int64(offset), nodeSize)
	if err != nil {
		return fmt.Errorf("pst: reading index node at %d: %w", offset, err)
	}
	if len(data) < nodeSize {
		return fmt.Errorf("pst: index node at %d is truncated", offset)
	}

	var numEntries, entrySize, level uint8
	entryLimit := 496
	if p.is64 {
		numEntries = data[488]
		entrySize = data[490]
		level = data[491]
		entryLimit = 488
	} else {
		numEntries = data[496]
		entrySize = data[498]
		level = data[499]
	}
	if entrySize == 0 {
		return nil
	}

	for i := 0; i < int(numEntries); i++ {
		at := i * int(entrySize)
		if at+int(entrySize) > entryLimit {
			break
		}

		if level == 0 {
			if offsetIndex {
				entry := offsetEntry{}
				if p.is64 {
					entry.identifier = binary.LittleEndian.Uint64(data[at : at+8])
					entry.fileOffset = binary.LittleEndian.Uint64(data[at+8 : at+16])
					entry.dataSize = uint32(binary.LittleEndian.Uint16(data[at+16 : at+18]))
					entry.refCount = uint32(binary.LittleEndian.Uint16(data[at+18 : at+20]))
				} else {
					entry.identifier = uint64(binary.LittleEndian.Uint32(data[at : at+4]))
					entry.fileOffset = uint64(binary.LittleEndian.Uint32(data[at+4 : at+8]))
					entry.dataSize = uint32(binary.LittleEndian.Uint16(data[at+8 : at+10]))
					entry.refCount = uint32(binary.LittleEndian.Uint16(data[at+10 : at+12]))
				}
				if entry.fileOffset == 0 || entry.dataSize == 0 {
					continue
				}
				p.offsets[entry.identifier] = entry
			} else {
				entry := descriptorEntry{}
				if p.is64 {
					entry.identifier = binary.LittleEndian.Uint64(data[at : at+8])
					entry.dataIdentifier = binary.LittleEndian.Uint64(data[at+8 : at+16])
					entry.localDescriptors = binary.LittleEndian.Uint64(data[at+16 : at+24])
					entry.parentIdentifier = binary.LittleEndian.Uint32(data[at+24 : at+28])
				} else {
					entry.identifier = uint64(binary.LittleEndian.Uint32(data[at : at+4]))
					entry.dataIdentifier = uint64(binary.LittleEndian.Uint32(data[at+4 : at+8]))
					entry.localDescriptors = uint64(binary.LittleEndian.Uint32(data[at+8 : at+12]))
					entry.parentIdentifier = binary.LittleEndian.Uint32(data[at+12 : at+16])
				}
				p.descriptors[entry.identifier] = entry
			}
			continue
		}

		// Branch entry: the last field is the file offset of the child node.
		var child uint64
		if p.is64 {
			child = binary.LittleEndian.Uint64(data[at+16 : at+24])
		} else {
			child = uint64(binary.LittleEndian.Uint32(data[at+8 : at+12]))
		}
		if child == 0 {
			continue
		}
		if err := p.walkIndex(child, offsetIndex, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// dataBlocks returns the table array entries for a data identifier. A block
// flagged internal is an array of data identifiers: the item's bytes are those
// blocks concatenated, in order, footers removed.
func (p *File) dataBlocks(id uint64) ([]block, error) {
	if cached, ok := p.blocks[id]; ok {
		return cached, nil
	}
	entry, ok := p.offsets[id]
	if !ok {
		return nil, fmt.Errorf("pst: no data block for identifier %d", id)
	}
	// The offsets index records the payload size. The block on disk is rounded
	// up to a 64 byte boundary, with the footer in the last few bytes; when the
	// rounding leaves less room than the footer, another boundary is added.
	footerSize := 12
	if p.is64 {
		footerSize = 16
	}
	blockSize := int(entry.dataSize)
	blockSize = ((blockSize + 63) / 64) * 64
	if blockSize-int(entry.dataSize) < footerSize {
		blockSize += 64
	}
	if size, err := p.rand.Size(); err == nil && int64(blockSize) > size {
		blockSize = int(size)
	}
	raw, err := p.rand.ReadAt(int64(entry.fileOffset), int64(blockSize))
	if err != nil {
		return nil, fmt.Errorf("pst: reading data block %d: %w", id, err)
	}

	internal := entry.identifier&flagInternal != 0
	if !internal {
		raw = decryptBlock(raw, p.encType, uint32(entry.identifier))
	}

	if len(raw) < footerSize {
		return nil, fmt.Errorf("pst: data block %d is smaller than its footer", id)
	}
	payloadSize := int(binary.LittleEndian.Uint16(raw[len(raw)-footerSize : len(raw)-footerSize+2]))
	if payloadSize < 0 || payloadSize > len(raw)-footerSize {
		payloadSize = len(raw) - footerSize
	}
	payload := raw[:payloadSize]

	if !internal {
		out := []block{{data: payload}}
		p.blocks[id] = out
		return out, nil
	}

	// An internal block is not data but an array of identifiers, but only when
	// it actually starts like one: signature 0x01, level 1 or 2. Some internal
	// blocks (a local descriptors root, for instance) are ordinary data whose
	// identifier merely carries the "do not decrypt" bit.
	if internal && len(payload) >= 2 && payload[0] == 0x01 && (payload[1] == 1 || payload[1] == 2) {
		level := payload[1]
		count := int(binary.LittleEndian.Uint16(payload[2:4]))
		if level == 0 {
			return nil, fmt.Errorf("pst: data array %d has no level", id)
		}
		entries := payload[8:]
		step := 4
		if p.is64 {
			step = 8
		}
		if count*step > len(entries) {
			count = len(entries) / step
		}

		out := []block{}
		for i := 0; i < count; i++ {
			at := i * step
			var sub uint64
			if p.is64 {
				sub = binary.LittleEndian.Uint64(entries[at : at+step])
			} else {
				sub = uint64(binary.LittleEndian.Uint32(entries[at : at+step]))
			}
			if sub == 0 {
				continue
			}
			nested, err := p.dataBlocks(sub)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
		}
		p.blocks[id] = out
		return out, nil
	}

	out := []block{{data: payload}}
	p.blocks[id] = out
	return out, nil
}

// decryptBlock undoes the per-block obfuscation. The two tables are the fixed
// permutations from [MS-PST]; "compressible" is one permutation, "high" adds a
// salt that walks forward through the block.
func decryptBlock(data []byte, encType uint8, key uint32) []byte {
	if encType == 0 {
		return data
	}

	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = compressibleTable[b]
	}
	if encType != 2 {
		return out
	}

	salt := uint16(((key & 0xffff0000) >> 16) ^ (key & 0x0000ffff))
	for i := range out {
		v := uint16(out[i])
		v = (v + (salt & 0xff)) & 0xff
		v = uint16(high1Table[v])
		v = (v + (salt >> 8)) & 0xff
		v = uint16(high2Table[v])
		v = (v - (salt >> 8)) & 0xff
		v = uint16(compressibleTable[v])
		v = (v - (salt & 0xff)) & 0xff
		out[i] = byte(v)
		salt++
	}
	return out
}

// tableFor reads the table held by an item's data stream.
func (p *File) tableFor(descriptor uint64) ([]block, error) {
	entry, ok := p.descriptors[descriptor]
	if !ok || entry.dataIdentifier == 0 {
		return nil, nil
	}
	blocks, err := p.dataBlocks(entry.dataIdentifier)
	if err != nil {
		return nil, err
	}
	for i := range blocks {
		blocks[i].index = parseBlockIndex(blocks[i].data)
	}
	return blocks, nil
}

// parseBlockIndex reads a table block index: the count of value boundaries,
// then the boundaries themselves. Value n runs from boundary n-1 to boundary n.
func parseBlockIndex(data []byte) []indexValue {
	if len(data) < 4 {
		return nil
	}
	indexOffset := int(binary.LittleEndian.Uint16(data[0:2]))
	if indexOffset == 0 || indexOffset+4 > len(data) {
		return nil
	}
	count := int(binary.LittleEndian.Uint16(data[indexOffset : indexOffset+2]))
	if count == 0 || indexOffset+4+(count+1)*2 > len(data) {
		return nil
	}

	boundaries := make([]uint16, count+1)
	for i := 0; i <= count; i++ {
		at := indexOffset + 4 + i*2
		boundaries[i] = binary.LittleEndian.Uint16(data[at : at+2])
	}

	values := make([]indexValue, 0, count)
	for i := 0; i < count; i++ {
		if boundaries[i] > boundaries[i+1] || boundaries[i+1] > uint16(len(data)) {
			continue
		}
		values = append(values, indexValue{start: boundaries[i], end: boundaries[i+1]})
	}
	return values
}

// tableValue resolves an internal table value reference: the high 16 bits pick
// the table array entry, bits 5..15 pick the index item inside it.
func tableValue(blocks []block, ref uint32) ([]byte, bool) {
	if ref&0x1f != 0 {
		return nil, false
	}
	arrayEntry := int(ref >> 16)
	valueIndex := int((ref&0x0000ffe0)>>5) - 1
	if arrayEntry < 0 || arrayEntry >= len(blocks) {
		return nil, false
	}
	index := blocks[arrayEntry].index
	if valueIndex < 0 || valueIndex >= len(index) {
		return nil, false
	}
	v := index[valueIndex]
	if v.end > uint16(len(blocks[arrayEntry].data)) {
		return nil, false
	}
	return blocks[arrayEntry].data[v.start:v.end], true
}

// recordEntry is one leaf of a table's b5 tree.
type recordEntry struct {
	identifier uint32
	valueType  uint16
	raw        uint32
}

// recordEntries walks a table's b5 record entry tree down to its leaves.
func recordEntries(blocks []block, ref uint32, level, idSize, valueSize, depth int) ([]recordEntry, error) {
	if ref == 0 || depth > 8 {
		return nil, nil
	}
	data, ok := tableValue(blocks, ref)
	if !ok {
		return nil, nil
	}

	if level == 0 {
		entrySize := idSize + valueSize
		if entrySize <= 0 || len(data) < entrySize {
			return nil, nil
		}
		out := make([]recordEntry, 0, len(data)/entrySize)
		for at := 0; at+entrySize <= len(data); at += entrySize {
			entry := recordEntry{}
			switch idSize {
			case 2:
				entry.identifier = uint32(binary.LittleEndian.Uint16(data[at : at+2]))
			case 4:
				entry.identifier = binary.LittleEndian.Uint32(data[at : at+4])
			case 8:
				entry.identifier = uint32(binary.LittleEndian.Uint64(data[at : at+8]))
			case 16:
				entry.identifier = binary.LittleEndian.Uint32(data[at : at+4])
			default:
				return nil, fmt.Errorf("pst: unsupported record entry identifier size %d", idSize)
			}
			// bc record entries are type(2) + value(4); 7c, 8c and ac carry only
			// the value in the trailing bytes.
			entry.raw = binary.LittleEndian.Uint32(data[at+entrySize-4 : at+entrySize])
			if idSize == 2 {
				entry.valueType = uint16(binary.LittleEndian.Uint16(data[at+2 : at+4]))
			}
			out = append(out, entry)
		}
		return out, nil
	}

	var out []recordEntry
	entrySize := idSize + 4
	for at := 0; at+entrySize <= len(data); at += entrySize {
		subRef := binary.LittleEndian.Uint32(data[at+idSize : at+idSize+4])
		sub, err := recordEntries(blocks, subRef, level-1, idSize, valueSize, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// properties returns the MAPI record entries of an item, or nil when the item
// holds no table.
func (p *File) properties(descriptor uint64) ([]recordEntry, error) {
	blocks, err := p.tableFor(descriptor)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 || len(blocks[0].data) < 4 || blocks[0].data[2] != 0xec {
		return nil, nil
	}

	tableType := blocks[0].data[3]
	valueRef := binary.LittleEndian.Uint32(blocks[0].data[4:8])

	b5Ref := valueRef
	if tableType != 0x8c && tableType != 0xa5 && tableType != 0xbc {
		header, ok := tableValue(blocks, valueRef)
		if !ok || len(header) < 12 {
			return nil, nil
		}
		switch tableType {
		case 0x6c, 0x9c:
			b5Ref = binary.LittleEndian.Uint32(header[0:4])
		case 0x7c, 0xac:
			b5Ref = binary.LittleEndian.Uint32(header[10:14])
		default:
			return nil, nil
		}
	}

	b5, ok := tableValue(blocks, b5Ref)
	if !ok || len(b5) < 8 || b5[0] != 0xb5 {
		return nil, nil
	}
	return recordEntries(blocks, binary.LittleEndian.Uint32(b5[4:8]), int(b5[3]), int(b5[1]), int(b5[2]), 0)
}

// localDescriptorData returns the raw bytes of one of an item's local
// descriptors: the overflow storage for property values too large for a record
// entry, such as bodies and attachment bytes.
func (p *File) localDescriptorData(descriptor uint64, identifier uint32) ([]byte, error) {
	blocks, err := p.localDescriptorBlocks(descriptor, identifier)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0)
	for _, b := range blocks {
		out = append(out, b.data...)
	}
	return out, nil
}
