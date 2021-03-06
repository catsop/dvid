/*
	This file supports serialization/deserialization and compression of data.
*/

package dvid

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	_ "log"

	"github.com/janelia-flyem/go/snappy-go/snappy"
)

// Compression is the format of compression for storing data
type Compression uint8

const (
	Uncompressed Compression = 0
	Snappy                   = 1 << iota
)

func (compress Compression) String() string {
	switch compress {
	case Uncompressed:
		return "No compression"
	case Snappy:
		return "Google's Snappy compression"
	default:
		return "Unknown compression"
	}
}

// Checksum is the type of checksum employed for error checking stored data
type Checksum uint8

const (
	NoChecksum Checksum = 0
	CRC32               = 1 << iota
)

func (checksum Checksum) String() string {
	switch checksum {
	case NoChecksum:
		return "No checksum"
	case CRC32:
		return "CRC32 checksum"
	default:
		return "Unknown checksum"
	}
}

// SerializationFormat is a single byte combining both compression and checksum methods.
type SerializationFormat uint8

func EncodeSerializationFormat(compress Compression, checksum Checksum) SerializationFormat {
	a := (uint8(compress) & 0x0F) << 4
	b := uint8(checksum) & 0x0F
	return SerializationFormat(a | b)
}

func DecodeSerializationFormat(s SerializationFormat) (compress Compression, checksum Checksum) {
	compress = Compression(uint8(s) >> 4)
	checksum = Checksum(uint8(s) & 0x0F)
	return
}

// Serialize a slice of bytes using optional compression, checksum
func SerializeData(data []byte, compress Compression, checksum Checksum) (s []byte, err error) {
	var buffer bytes.Buffer

	// Store the requested compression and checksum
	format := EncodeSerializationFormat(compress, checksum)
	err = binary.Write(&buffer, binary.LittleEndian, format)
	if err != nil {
		return
	}

	// Handle compression if requested
	var byteData []byte
	switch compress {
	case Uncompressed:
		byteData = data
	case Snappy:
		byteData, err = snappy.Encode(nil, data)
	default:
		err = fmt.Errorf("Illegal compression (%s) during serialization", compress)
	}
	if err != nil {
		return
	}

	// Handle checksum if requested
	switch checksum {
	case NoChecksum:
	case CRC32:
		crcChecksum := crc32.ChecksumIEEE(byteData)
		err = binary.Write(&buffer, binary.LittleEndian, crcChecksum)
	default:
		err = fmt.Errorf("Illegal checksum (%s) in serialize.SerializeData()", checksum)
	}
	if err == nil {
		// Note the actual data is written last, after any checksum so we don't have to
		// worry about length when deserializing.
		_, err = buffer.Write(byteData)
		if err == nil {
			s = buffer.Bytes()
		}
	}
	return
}

// Serializes an arbitrary Go object using Gob encoding and optional compression, checksum.
// If your object is []byte, you should preferentially use SerializeData since the Gob encoding
// process adds some overhead in performance as well as size of wire format to describe the
// transmitted types.
func Serialize(object interface{}, compress Compression, checksum Checksum) ([]byte, error) {
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	err := enc.Encode(object)
	if err != nil {
		return nil, err
	}
	return SerializeData(buffer.Bytes(), compress, checksum)
}

// DeserializeData deserializes a slice of bytes using stored compression, checksum.
// If uncompress parameter is false, the data is not uncompressed.
func DeserializeData(s []byte, uncompress bool) (data []byte, compress Compression, err error) {
	buffer := bytes.NewBuffer(s)

	// Get the stored compression and checksum
	var format SerializationFormat
	err = binary.Read(buffer, binary.LittleEndian, &format)
	if err != nil {
		return
	}
	var checksum Checksum
	compress, checksum = DecodeSerializationFormat(format)

	// Get any checksum.
	var storedCrc32 uint32
	switch checksum {
	case NoChecksum:
	case CRC32:
		err = binary.Read(buffer, binary.LittleEndian, &storedCrc32)
	default:
		err = fmt.Errorf("Illegal checksum in deserializing data")
	}
	if err != nil {
		return
	}

	// Get the possibly compressed data.
	data = buffer.Bytes()

	// Perform any requested checksum
	switch checksum {
	case CRC32:
		crcChecksum := crc32.ChecksumIEEE(data)
		if crcChecksum != storedCrc32 {
			err = fmt.Errorf("Bad checksum.  Stored %x got %x", storedCrc32, crcChecksum)
		}
	}

	// Uncompress if needed
	if uncompress {
		switch compress {
		case Uncompressed:
		case Snappy:
			data, err = snappy.Decode(nil, data)
		default:
			err = fmt.Errorf("Illegal compressiont format (%d) in deserialization", compress)
		}
	}
	return
}

// Deserializes a Go object using Gob encoding
func Deserialize(s []byte, object interface{}) error {
	// Get the bytes for the Gob-encoded object
	data, _, err := DeserializeData(s, true)
	if err != nil {
		return err
	}

	// Decode the bytes
	buffer := bytes.NewBuffer(data)
	dec := gob.NewDecoder(buffer)
	return dec.Decode(object)
}
