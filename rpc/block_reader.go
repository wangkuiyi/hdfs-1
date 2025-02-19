package rpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"net"

	hdfs "github.com/colinmarc/hdfs/protocol/hadoop_hdfs"
	"github.com/golang/protobuf/proto"
)

// BlockReader implements io.ReadCloser, for reading a block. It abstracts over
// reading from multiple datanodes, in order to be robust to connection
// failures, timeouts, and other shenanigans.
type BlockReader struct {
	block     *hdfs.LocatedBlockProto
	datanodes *datanodeFailover
	stream    *blockReadStream
	conn      net.Conn
	offset    int64
	closed    bool
}

// NewBlockReader returns a new BlockReader, given the block information and
// security token from the namenode. It will connect (lazily) to one of the
// provided datanode locations based on which datanodes have seen failures.
func NewBlockReader(block *hdfs.LocatedBlockProto, offset int64) *BlockReader {
	locs := block.GetLocs()
	datanodes := make([]string, len(locs))
	for i, loc := range locs {
		dn := loc.GetId()
		datanodes[i] = fmt.Sprintf("%s:%d", dn.GetIpAddr(), dn.GetXferPort())
	}

	return &BlockReader{
		block:     block,
		datanodes: newDatanodeFailover(datanodes),
		offset:    offset,
	}
}

// Read implements io.Reader.
//
// In the case that a failure (such as a disconnect) occurs while reading, the
// BlockReader will failover to another datanode and continue reading
// transparently. In the case that all the datanodes fail, the error
// from the most recent attempt will be returned.
//
// Any datanode failures are recorded in a global cache, so subsequent reads,
// even reads for different blocks, will prioritize them lower.
func (br *BlockReader) Read(b []byte) (int, error) {
	if br.closed {
		return 0, io.ErrClosedPipe
	} else if uint64(br.offset) >= br.block.GetB().GetNumBytes() {
		br.Close()
		return 0, io.EOF
	}

	// This is the main retry loop.
	for br.stream != nil || br.datanodes.numRemaining() > 0 {
		// First, we try to connect. If this fails, we can just skip the datanode
		// and continue.
		if br.stream == nil {
			err := br.connectNext()
			if err != nil {
				br.datanodes.recordFailure(err)
				continue
			}
		}

		// Then, try to read. If we fail here after reading some bytes, we return
		// a partial read (n < len(b)).
		n, err := br.stream.Read(b)
		br.offset += int64(n)
		if err != nil && err != io.EOF {
			br.stream = nil
			br.datanodes.recordFailure(err)
			if n > 0 {
				return n, nil
			}

			continue
		}

		return n, err
	}

	err := br.datanodes.lastError()
	if err == nil {
		err = errors.New("No available datanodes for block.")
	}

	return 0, err
}

// Close implements io.Closer.
func (br *BlockReader) Close() error {
	br.closed = true
	if br.conn != nil {
		br.conn.Close()
	}

	return nil
}

// connectNext pops a datanode from the list based on previous failures, and
// connects to it.
func (br *BlockReader) connectNext() error {
	address := br.datanodes.next()

	conn, err := net.DialTimeout("tcp", address, connectionTimeout)
	if err != nil {
		return err
	}

	err = br.writeBlockReadRequest(conn)
	if err != nil {
		return err
	}

	resp, err := br.readBlockReadResponse(conn)
	if err != nil {
		return err
	}

	readInfo := resp.GetReadOpChecksumInfo()
	checksumInfo := readInfo.GetChecksum()

	var checksumTab *crc32.Table
	checksumType := checksumInfo.GetType()
	switch checksumType {
	case hdfs.ChecksumTypeProto_CHECKSUM_CRC32:
		checksumTab = crc32.IEEETable
	case hdfs.ChecksumTypeProto_CHECKSUM_CRC32C:
		checksumTab = crc32.MakeTable(crc32.Castagnoli)
	default:
		return fmt.Errorf("Unsupported checksum type: %d", checksumType)
	}

	chunkSize := int(checksumInfo.GetBytesPerChecksum())
	stream := newBlockReadStream(conn, chunkSize, checksumTab)

	// The read will start aligned to a chunk boundary, so we need to seek forward
	// to the requested offset.
	amountToDiscard := br.offset - int64(readInfo.GetChunkOffset())
	if amountToDiscard > 0 {
		_, err := io.CopyN(ioutil.Discard, stream, amountToDiscard)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}

			conn.Close()
			return err
		}
	}

	br.stream = stream
	br.conn = conn
	return nil
}

// A read request to a datanode:
// +-----------------------------------------------------------+
// |  Data Transfer Protocol Version, int16                    |
// +-----------------------------------------------------------+
// |  Op code, 1 byte (READ_BLOCK = 0x51)                      |
// +-----------------------------------------------------------+
// |  varint length + OpReadBlockProto                         |
// +-----------------------------------------------------------+
func (br *BlockReader) writeBlockReadRequest(w io.Writer) error {
	header := []byte{0x00, dataTransferVersion, readBlockOp}

	needed := br.block.GetB().GetNumBytes() - uint64(br.offset)
	op := newBlockReadOp(br.block, uint64(br.offset), needed)
	opBytes, err := makeDelimitedMsg(op)
	if err != nil {
		return err
	}

	req := append(header, opBytes...)
	_, err = w.Write(req)
	if err != nil {
		return err
	}

	return nil
}

// The initial response from the datanode:
// +-----------------------------------------------------------+
// |  varint length + BlockOpResponseProto                     |
// +-----------------------------------------------------------+
func (br *BlockReader) readBlockReadResponse(r io.Reader) (*hdfs.BlockOpResponseProto, error) {
	varintBytes := make([]byte, binary.MaxVarintLen32)
	_, err := io.ReadFull(r, varintBytes)
	if err != nil {
		return nil, err
	}

	respLength, varintLength := binary.Uvarint(varintBytes)
	if varintLength < 1 {
		return nil, io.ErrUnexpectedEOF
	}

	// We may have grabbed too many bytes when reading the varint.
	respBytes := make([]byte, respLength)
	extraLength := copy(respBytes, varintBytes[varintLength:])
	_, err = io.ReadFull(r, respBytes[extraLength:])
	if err != nil {
		return nil, err
	}

	resp := &hdfs.BlockOpResponseProto{}
	err = proto.Unmarshal(respBytes, resp)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func newBlockReadOp(block *hdfs.LocatedBlockProto, offset, length uint64) *hdfs.OpReadBlockProto {
	return &hdfs.OpReadBlockProto{
		Header: &hdfs.ClientOperationHeaderProto{
			BaseHeader: &hdfs.BaseHeaderProto{
				Block: block.GetB(),
				Token: block.GetBlockToken(),
			},
			ClientName: proto.String(ClientName),
		},
		Offset: proto.Uint64(offset),
		Len:    proto.Uint64(length),
	}
}
