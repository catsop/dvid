/*
	Package voxels implements DVID support for data using voxels as elements.
*/
package voxels

import (
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version = "0.8"
	RepoUrl = "github.com/janelia-flyem/dvid/datatype/voxels"

	// Don't allow requests for more than this many voxels, which is larger than
	// 1000 x 1000 x 1000 volume, or 30000 x 30000 image.
	MaxVoxelsRequest = dvid.Giga
)

const HelpMessage = `
API for 'voxels' datatype (github.com/janelia-flyem/dvid/datatype/voxels)
=========================================================================

Command-line:

$ dvid dataset <UUID> new <type name> <data name> <settings...>

	Adds newly named data of the 'type name' to dataset with specified UUID.

	Example:

	$ dvid dataset 3f8c new grayscale8 mygrayscale BlockSize=32 Res=1.5,1.0,1.5

    Arguments:

    UUID           Hexidecimal string with enough characters to uniquely identify a version node.
    type name      Data type name, e.g., "grayscale8"
    data name      Name of data to create, e.g., "mygrayscale"
    settings       Configuration settings in "key=value" format separated by spaces.

    Configuration Settings (case-insensitive keys)

    Versioned      "true" or "false" (default)
    BlockSize      Size in pixels  (default: %s)
    Res       Resolution of voxels (default: 1.0, 1.0, 1.0)
    Units  String of units (default: "nanometers")

$ dvid node <UUID> <data name> load <offset> <image glob>

    Initializes version node to a set of XY images described by glob of filenames.  The
    DVID server must have access to the named files.  Currently, XY images are required.

    Example: 

    $ dvid node 3f8c mygrayscale load 0,0,100 data/*.png

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    offset        3d coordinate in the format "x,y,z".  Gives coordinate of top upper left voxel.
    image glob    Filenames of images, e.g., foo-xy-*.png

$ dvid node <UUID> <data name> put local  <plane> <offset> <image glob>
$ dvid node <UUID> <data name> put remote <plane> <offset> <image glob>

    Adds image data to a version node when the server can see the local files ("local")
    or when the server must be sent the files via rpc ("remote").  If possible, use the
    "load" command instead because it is much more efficient.

    Example: 

    $ dvid node 3f8c mygrayscale put local xy 0,0,100 data/*.png

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    dims          The axes of data extraction in form "i,j,k,..."  Example: "0,2" can be XZ.
                    Slice strings ("xy", "xz", or "yz") are also accepted.
    offset        3d coordinate in the format "x,y,z".  Gives coordinate of top upper left voxel.
    image glob    Filenames of images, e.g., foo-xy-*.png
	
    ------------------

HTTP API (Level 2 REST):

GET  /api/node/<UUID>/<data name>/help

	Returns data-specific help message.


GET  /api/node/<UUID>/<data name>/info
POST /api/node/<UUID>/<data name>/info

    Retrieves or puts DVID-specific data properties for these voxels.

    Example: 

    GET /api/node/3f8c/grayscale/info

    Returns JSON with configuration settings that include location in DVID space and
    min/max block indices.

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of voxels data.


GET  /api/node/<UUID>/<data name>/schema

	Retrieves a JSON schema (application/vnd.dvid-nd-data+json) that describes the layout
	of bytes returned for n-d images.


GET  /api/node/<UUID>/<data name>/<dims>/<size>/<offset>[/<format>]
POST /api/node/<UUID>/<data name>/<dims>/<size>/<offset>[/<format>]

    Retrieves or puts voxel data.

    Example: 

    GET /api/node/3f8c/grayscale/0_1/512_256/0_0_100/jpg:80

    Returns an XY slice (0th and 1st dimensions) with width (x) of 512 voxels and
    height (y) of 256 voxels with offset (0,0,100) in JPG format with quality 80.
    The example offset assumes the "grayscale" data in version node "3f8c" is 3d.
    The "Content-type" of the HTTP response should agree with the requested format.
    For example, returned PNGs will have "Content-type" of "image/png", and returned
    nD data will be "application/octet-stream".

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    dims          The axes of data extraction in form "i_j_k,..."  Example: "0_2" can be XZ.
                    Slice strings ("xy", "xz", or "yz") are also accepted.
    size          Size in voxels along each dimension specified in <dims>.
    offset        Gives coordinate of first voxel using dimensionality of data.
    format        Valid formats depend on the dimensionality of the request and formats
                    available in server implementation.
                  2D: "png", "jpg" (default: "png")
                    jpg allows lossy quality setting, e.g., "jpg:80"
                  nD: uses default "octet-stream".

(TO DO)

GET  /api/node/<UUID>/<data name>/arb/<center>/<normal>/<size>[/<format>]

    Retrieves non-orthogonal (arbitrarily oriented planar) image data of named 3d data 
    within a version node.

    Example: 

    GET /api/node/3f8c/grayscale/arb/200_200/2.0_1.3_1/100_100/jpg:80

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    center        3d coordinate in the format "x_y_z".  Gives 3d coord of center pixel.
    normal        3d vector in the format "nx_ny_nz".  Gives normal vector of image.
    size          Size in pixels in the format "dx_dy".
    format        "png", "jpg" (default: "png")  
                    jpg allows lossy quality setting, e.g., "jpg:80"
`

var (
	// DefaultBlockSize specifies the default size for each block of this data type.
	DefaultBlockSize int32 = 32

	DefaultRes float32 = 10

	DefaultUnits = "nanometers"
)

func init() {
	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Datatype{})
	gob.Register(&Data{})
	gob.Register(&binary.LittleEndian)
	gob.Register(&binary.BigEndian)
}

// Operation holds Voxel-specific data for processing chunks.
type Operation struct {
	ExtHandler
	OpType
}

type OpType int

const (
	GetOp OpType = iota
	PutOp
)

func (o OpType) String() string {
	switch o {
	case GetOp:
		return "Get Op"
	case PutOp:
		return "Put Op"
	default:
		return "Illegal Op"
	}
}

// Block is the basic key/value for the voxel type.
// The value is a slice of bytes corresponding to data within a block.
type Block storage.KeyValue

// Blocks is a slice of Block.
type Blocks []Block

// IntHandler implementations handle internal DVID voxel representations, knowing how
// to break data into chunks (blocks for voxels).  Typically, each voxels-oriented
// package has a Data type that fulfills the IntHandler interface.
type IntHandler interface {
	NewExtHandler(dvid.Geometry, interface{}) (ExtHandler, error)

	DataID() datastore.DataID

	Values() DataValues

	BlockSize() dvid.Point

	Extents() *Extents

	VersionMutex(dvid.VersionLocalID) *sync.Mutex

	ProcessChunk(*storage.Chunk)
}

// ExtHandler provides the shape, location (indexing), and data of a set of voxels
// connected with external usage. It is the type used for I/O from DVID to clients,
// e.g., 2d images, 3d subvolumes, etc.  These user-facing data must be converted to
// and from internal DVID representations using key/value pairs where the value is a
// block of data, and the key contains some spatial indexing.
//
// We can read/write different external formats through the following steps:
// 1) Create a data type package (e.g., datatype/labels64) and define a ExtHandler type
//    where the data layout (i.e., the values in a voxel) is identical to
//    the targeted DVID IntHandler.
// 2) Do I/O for external format (e.g., Raveler's superpixel PNG images with implicit Z)
//    and convert external data to the ExtHandler instance.
// 3) Pass ExtHandler to voxels package-level functions.
//
type ExtHandler interface {
	dvid.Geometry

	Values() DataValues

	Stride() int32

	ByteOrder() binary.ByteOrder

	Data() []byte

	Index(p dvid.ChunkPoint) dvid.Index

	IndexIterator(chunkSize dvid.Point) (dvid.IndexIterator, error)

	//Returns a image.Image suitable for external DVID use
	GoImage() (image.Image, error)
}

// GetImage retrieves a 2d Go image from a version node given a geometry of voxels.
func GetImage(uuid dvid.UUID, i IntHandler, e ExtHandler) (image.Image, error) {
	if err := GetVoxels(uuid, i, e); err != nil {
		return nil, err
	}
	return e.GoImage()
}

// GetVolume retrieves a n-d volume from a version node given a geometry of voxels.
func GetVolume(uuid dvid.UUID, i IntHandler, e ExtHandler) ([]byte, error) {
	if err := GetVoxels(uuid, i, e); err != nil {
		return nil, err
	}
	return e.Data(), nil
}

// GetVoxels retrieves voxels from a version node and stores them in the ExtHandler.
func GetVoxels(uuid dvid.UUID, i IntHandler, e ExtHandler) error {
	db := server.StorageEngine()
	if db == nil {
		return fmt.Errorf("Did not find a working key-value datastore to get image!")
	}

	op := Operation{e, GetOp}
	wg := new(sync.WaitGroup)
	chunkOp := &storage.ChunkOp{&op, wg}

	service := server.DatastoreService()
	_, versionID, err := service.LocalIDFromUUID(uuid)

	dataID := i.DataID()

	for it, err := e.IndexIterator(i.BlockSize()); err == nil && it.Valid(); it.NextSpan() {
		indexBeg, indexEnd, err := it.IndexSpan()
		if err != nil {
			return err
		}
		startKey := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, indexBeg}
		endKey := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, indexEnd}

		// Send the entire range of key/value pairs to ProcessChunk()
		err = db.ProcessRange(startKey, endKey, chunkOp, i.ProcessChunk)
		if err != nil {
			return fmt.Errorf("Unable to GET data %s: %s", dataID.DataName(), err.Error())
		}
	}
	if err != nil {
		return err
	}

	// Reduce: Grab the resulting 2d image.
	wg.Wait()
	return nil
}

// PutLocal adds image data to a version node, altering underlying blocks if the image
// intersects the block.
//
// The image filename glob MUST BE absolute file paths that are visible to the server.
// This function is meant for mass ingestion of large data files, and it is inappropriate
// to read gigabytes of data just to send it over the network to a local DVID.
func (d *Data) PutLocal(request datastore.Request, reply *datastore.Response) error {
	startTime := time.Now()

	// Parse the request
	var uuidStr, dataName, cmdStr, sourceStr, planeStr, offsetStr string
	filenames := request.CommandArgs(1, &uuidStr, &dataName, &cmdStr, &sourceStr,
		&planeStr, &offsetStr)
	if len(filenames) == 0 {
		return fmt.Errorf("Need to include at least one file to add: %s", request)
	}

	// Get offset
	offset, err := dvid.StringToPoint(offsetStr, ",")
	if err != nil {
		return fmt.Errorf("Illegal offset specification: %s: %s", offsetStr, err.Error())
	}

	// Get list of files to add
	var addedFiles string
	if len(filenames) == 1 {
		addedFiles = filenames[0]
	} else {
		addedFiles = fmt.Sprintf("filenames: %s [%d more]", filenames[0], len(filenames)-1)
	}
	dvid.Log(dvid.Debug, addedFiles+"\n")

	// Get plane
	plane, err := dvid.DataShapeString(planeStr).DataShape()
	if err != nil {
		return err
	}

	// Load and PUT each image.
	uuid, err := server.MatchingUUID(uuidStr)
	if err != nil {
		return err
	}

	numSuccessful := 0
	for _, filename := range filenames {
		sliceTime := time.Now()
		img, _, err := dvid.ImageFromFile(filename)
		if err != nil {
			return fmt.Errorf("Error after %d images successfully added: %s",
				numSuccessful, err.Error())
		}
		slice, err := dvid.NewOrthogSlice(plane, offset, dvid.RectSize(img.Bounds()))
		if err != nil {
			return fmt.Errorf("Unable to determine slice: %s", err.Error())
		}

		e, err := d.NewExtHandler(slice, img)
		if err != nil {
			return err
		}
		storage.FileBytesRead <- len(e.Data())
		err = PutImage(uuid, d, e)
		if err != nil {
			return err
		}
		dvid.ElapsedTime(dvid.Debug, sliceTime, "%s put local %s", d.DataName(), slice)
		numSuccessful++
		offset = offset.Add(dvid.Point3d{0, 0, 1})
	}
	dvid.ElapsedTime(dvid.Debug, startTime, "RPC put local (%s) completed", addedFiles)
	return nil
}

// PutImage adds a 2d image within given geometry to a version node.   Since chunk sizes
// are larger than a 2d slice, this also requires integrating this image into current
// chunks before writing result back out, so it's a PUT for nonexistant keys and GET/PUT
// for existing keys.
func PutImage(uuid dvid.UUID, i IntHandler, e ExtHandler) error {
	service := server.DatastoreService()
	_, versionID, err := service.LocalIDFromUUID(uuid)
	if err != nil {
		return err
	}

	db := server.StorageEngine()
	if db == nil {
		return fmt.Errorf("Did not find a working key-value datastore to put image!")
	}

	op := Operation{e, PutOp}
	wg := new(sync.WaitGroup)
	chunkOp := &storage.ChunkOp{&op, wg}

	// We only want one PUT on given version for given data to prevent interleaved
	// chunk PUTs that could potentially overwrite slice modifications.
	versionMutex := i.VersionMutex(versionID)
	versionMutex.Lock()
	defer versionMutex.Unlock()

	// Keep track of changing extents and mark dataset as dirty if changed.
	var extentChanged bool
	defer func() {
		if extentChanged {
			err := service.SaveDataset(uuid)
			if err != nil {
				dvid.Log(dvid.Normal, "Error in trying to save dataset on change: %s\n", err.Error())
			}
		}
	}()

	// Track point extents
	extents := i.Extents()
	if extents.AdjustPoints(e.StartPoint(), e.EndPoint()) {
		extentChanged = true
	}
	dataID := i.DataID()

	// Iterate through index space for this data.
	for it, err := e.IndexIterator(i.BlockSize()); err == nil && it.Valid(); it.NextSpan() {
		i0, i1, err := it.IndexSpan()
		if err != nil {
			return err
		}
		ptBeg := i0.Duplicate().(dvid.PointIndexer)
		ptEnd := i1.Duplicate().(dvid.PointIndexer)

		begX := ptBeg.Value(0)
		endX := ptEnd.Value(0)

		if extents.AdjustIndices(ptBeg, ptEnd) {
			extentChanged = true
		}

		startKey := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, ptBeg}
		endKey := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, ptEnd}

		// GET all the chunks for this range.
		keyvalues, err := db.GetRange(startKey, endKey)
		if err != nil {
			return fmt.Errorf("Error in reading data during PUT %s: %s", dataID.DataName(), err.Error())
		}

		// Send all data to chunk handlers for this range.
		var kv, oldkv storage.KeyValue
		numOldkv := len(keyvalues)
		oldI := 0
		if numOldkv > 0 {
			oldkv = keyvalues[oldI]
		}
		wg.Add(int(endX-begX) + 1)
		c := dvid.ChunkPoint3d{begX, ptBeg.Value(1), ptBeg.Value(2)}
		for x := begX; x <= endX; x++ {
			c[0] = x
			key := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, e.Index(c)}
			// Check for this key among old key-value pairs and if so,
			// send the old value into chunk handler.
			if oldkv.K != nil {
				indexer, err := datastore.KeyToPointIndexer(oldkv.K)
				if err != nil {
					return err
				}
				if indexer.Value(0) == x {
					kv = oldkv
					oldI++
					if oldI < numOldkv {
						oldkv = keyvalues[oldI]
					} else {
						oldkv.K = nil
					}
				} else {
					kv = storage.KeyValue{K: key}
				}
			} else {
				kv = storage.KeyValue{K: key}
			}
			// TODO -- Pass batch write via chunkOp and group all PUTs
			// together at once.  Should increase write speed, particularly
			// since the PUTs are using mostly sequential keys.
			i.ProcessChunk(&storage.Chunk{chunkOp, kv})
		}
	}
	wg.Wait()

	return nil
}

// Loads a XY oriented image at given offset, returning an ExtHandler.
func loadXYImage(i IntHandler, filename string, offset dvid.Point) (ExtHandler, error) {
	img, _, err := dvid.ImageFromFile(filename)
	if err != nil {
		return nil, err
	}
	slice, err := dvid.NewOrthogSlice(dvid.XY, offset, dvid.RectSize(img.Bounds()))
	if err != nil {
		return nil, fmt.Errorf("Unable to determine slice: %s", err.Error())
	}
	e, err := i.NewExtHandler(slice, img)
	if err != nil {
		return nil, err
	}
	storage.FileBytesRead <- len(e.Data())
	return e, nil
}

// Optimized bulk loading of XY images by loading all slices for a block before processing.
// Trades off memory for speed.
func LoadXY(i IntHandler, uuid dvid.UUID, offset dvid.Point, filenames []string) error {
	if len(filenames) == 0 {
		return nil
	}
	startTime := time.Now()

	service := server.DatastoreService()
	_, versionID, err := service.LocalIDFromUUID(uuid)
	if err != nil {
		return err
	}

	// We only want one PUT on given version for given data to prevent interleaved
	// chunk PUTs that could potentially overwrite slice modifications.
	versionMutex := i.VersionMutex(versionID)
	versionMutex.Lock()

	// Keep track of changing extents and mark dataset as dirty if changed.
	var extentChanged dvid.Bool

	// Handle cleanup given multiple goroutines still writing data.
	var writeWait, blockWait sync.WaitGroup
	defer func() {
		writeWait.Wait()
		versionMutex.Unlock()

		if extentChanged.Value() {
			err := service.SaveDataset(uuid)
			if err != nil {
				dvid.Log(dvid.Normal, "Error in trying to save dataset on change: %s\n", err.Error())
			}
		}
	}()

	// Load first slice, get dimensions, allocate blocks for whole slice.
	// Note: We don't need to lock the block slices because goroutines do NOT
	// access the same elements of a slice.
	var numBlocks int
	var blocks [2]Blocks
	curBlocks := 0
	blockSize := i.BlockSize()
	blockBytes := blockSize.Prod() * int64(i.Values().BytesPerVoxel())

	// Iterate through XY slices batched into the Z length of blocks.
	fileNum := 1
	for _, filename := range filenames {
		sliceTime := time.Now()

		zInBlock := offset.Value(2) % blockSize.Value(2)
		firstSlice := fileNum == 1
		lastSlice := fileNum == len(filenames)
		firstSliceInBlock := firstSlice || zInBlock == 0
		lastSliceInBlock := lastSlice || zInBlock == blockSize.Value(2)-1
		lastBlocks := fileNum+int(blockSize.Value(2)) > len(filenames)

		// Load images synchronously
		e, err := loadXYImage(i, filename, offset)
		if err != nil {
			return err
		}

		// Allocate blocks and/or load old block data if first/last XY blocks.
		// Note: Slices are only zeroed out on first and last slice with assumption
		// that ExtHandler is packed in XY footprint (values cover full extent).
		// If that is NOT the case, we need to zero out blocks for each block layer.
		if fileNum == 1 || (lastBlocks && firstSliceInBlock) {
			numBlocks = dvid.GetNumBlocks(e, blockSize)
			if fileNum == 1 {
				blocks[0] = make(Blocks, numBlocks, numBlocks)
				blocks[1] = make(Blocks, numBlocks, numBlocks)
				for i := 0; i < numBlocks; i++ {
					blocks[0][i].V = make([]byte, blockBytes, blockBytes)
					blocks[1][i].V = make([]byte, blockBytes, blockBytes)
				}
			} else {
				blocks[curBlocks] = make(Blocks, numBlocks, numBlocks)
				for i := 0; i < numBlocks; i++ {
					blocks[curBlocks][i].V = make([]byte, blockBytes, blockBytes)
				}
			}
			err = loadOldBlocks(i, e, blocks[curBlocks], versionID)
			if err != nil {
				return err
			}
		}

		// Transfer data between external<->internal blocks asynchronously
		blockWait.Add(1)
		go func(ext ExtHandler) {
			// Track point extents
			if i.Extents().AdjustPoints(e.StartPoint(), e.EndPoint()) {
				extentChanged.SetTrue()
			}

			// Process an XY image (slice).
			changed, err := writeXYImage(i, ext, blocks[curBlocks], versionID)
			if err != nil {
				dvid.Log(dvid.Normal, "Error writing XY image: %s\n", err.Error())
			}
			if changed {
				extentChanged.SetTrue()
			}
			blockWait.Done()
		}(e)

		// If this is the end of a block (or filenames), wait until all goroutines complete,
		// then asynchronously write blocks.
		if lastSliceInBlock {
			blockWait.Wait()
			if err = AsyncWriteData(blocks[curBlocks], &writeWait); err != nil {
				return err
			}
			curBlocks = (curBlocks + 1) % 2
		}

		fileNum++
		offset = offset.Add(dvid.Point3d{0, 0, 1})
		dvid.ElapsedTime(dvid.Debug, sliceTime, "Loaded %s slice %s", i, e)
	}
	dvid.ElapsedTime(dvid.Debug, startTime, "RPC load of %d files completed", len(filenames))

	return nil
}

// Loads blocks with old data if they exist.
func loadOldBlocks(i IntHandler, e ExtHandler, blocks Blocks, versionID dvid.VersionLocalID) error {
	db := server.StorageEngine()
	if db == nil {
		return fmt.Errorf("Did not find a working key-value datastore to put image!")
	}

	// Create a map of old blocks indexed by the index
	oldBlocks := map[string]([]byte){}

	// Iterate through index space for this data using ZYX ordering.
	dataID := i.DataID()
	blockSize := i.BlockSize()
	blockNum := 0
	for it, err := e.IndexIterator(blockSize); err == nil && it.Valid(); it.NextSpan() {
		indexBeg, indexEnd, err := it.IndexSpan()
		if err != nil {
			return err
		}

		// Get previous data.
		keyBeg := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, indexBeg}
		keyEnd := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, indexEnd}
		keyvalues, err := db.GetRange(keyBeg, keyEnd)
		if err != nil {
			return err
		}
		for _, kv := range keyvalues {
			dataKey, ok := kv.K.(*datastore.DataKey)
			if ok {
				block, _, err := dvid.DeserializeData(kv.V, true)
				if err != nil {
					return fmt.Errorf("Unable to deserialize block in '%s': %s",
						dataID.DataName(), err.Error())
				}
				oldBlocks[dataKey.Index.String()] = block
			} else {
				return fmt.Errorf("Error no DataKey in retrieving old data for loadOldBlocks()")
			}
		}

		ptBeg := indexBeg.Duplicate().(dvid.PointIndexer)
		ptEnd := indexEnd.Duplicate().(dvid.PointIndexer)
		begX := ptBeg.Value(0)
		endX := ptEnd.Value(0)
		c := dvid.ChunkPoint3d{begX, ptBeg.Value(1), ptBeg.Value(2)}
		for x := begX; x <= endX; x++ {
			c[0] = x
			key := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, e.Index(c)}
			blocks[blockNum].K = key
			block, ok := oldBlocks[key.Index.String()]
			if ok {
				copy(blocks[blockNum].V, block)
			}
			blockNum++
		}
	}
	return nil
}

// Writes a XY image (the ExtHandler) into the blocks that intersect it.
// This function assumes the blocks have been allocated and if necessary, filled
// with old data.
func writeXYImage(i IntHandler, e ExtHandler, blocks Blocks, versionID dvid.VersionLocalID) (extentChanged bool, err error) {
	db := server.StorageEngine()
	if db == nil {
		return false, fmt.Errorf("Did not find a working key-value datastore to put image!")
	}

	// Iterate through index space for this data using ZYX ordering.
	dataID := i.DataID()
	blockSize := i.BlockSize()
	blockNum := 0
	for it, err := e.IndexIterator(blockSize); err == nil && it.Valid(); it.NextSpan() {
		indexBeg, indexEnd, err := it.IndexSpan()
		if err != nil {
			return extentChanged, err
		}

		ptBeg := indexBeg.Duplicate().(dvid.PointIndexer)
		ptEnd := indexEnd.Duplicate().(dvid.PointIndexer)

		// Track point extents
		if i.Extents().AdjustIndices(ptBeg, ptEnd) {
			extentChanged = true
		}

		begX := ptBeg.Value(0)
		endX := ptEnd.Value(0)
		c := dvid.ChunkPoint3d{begX, ptBeg.Value(1), ptBeg.Value(2)}
		for x := begX; x <= endX; x++ {
			c[0] = x
			key := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, e.Index(c)}
			blocks[blockNum].K = key

			// Write this slice data into the block.
			WriteToBlock(e, &(blocks[blockNum]), blockSize)
			blockNum++
		}
	}
	return
}

// KVWriteSize is the # of key/value pairs we will write as one atomic batch write.
const KVWriteSize = 500

// AsyncWriteData writes blocks of voxel data asynchronously using batch writes.
func AsyncWriteData(blocks Blocks, wg *sync.WaitGroup) error {
	db := server.StorageEngine()
	if db == nil {
		return fmt.Errorf("Did not find a working key-value datastore to put image!")
	}

	wg.Wait()
	wg.Add(1)
	<-server.HandlerToken
	go func() {
		defer func() {
			server.HandlerToken <- 1
			wg.Done()
		}()
		// If we can do write batches, use it, else do put ranges.
		// With write batches, we write the byte slices immediately.
		// The put range approach can lead to duplicated memory.
		batcher, ok := db.(storage.Batcher)
		if ok {
			batch := batcher.NewBatch()
			for i, block := range blocks {
				serialization, err := dvid.SerializeData(block.V, dvid.Snappy, dvid.CRC32)
				if err != nil {
					fmt.Printf("Unable to serialize block: %s\n", err.Error())
					return
				}
				batch.Put(block.K, serialization)
				if i%KVWriteSize == KVWriteSize-1 || i == len(blocks)-1 {
					if err := batch.Commit(); err != nil {
						fmt.Printf("Error on trying to write batch: %s\n", err.Error())
						return
					}
					batch.Clear()
				}
			}
			batch.Close()
		} else {
			// Serialize and compress the blocks.
			keyvalues := make(storage.KeyValues, len(blocks))
			for i, block := range blocks {
				serialization, err := dvid.SerializeData(block.V, dvid.Snappy, dvid.CRC32)
				if err != nil {
					fmt.Printf("Unable to serialize block: %s\n", err.Error())
					return
				}
				keyvalues[i] = storage.KeyValue{
					K: block.K,
					V: serialization,
				}
			}

			// Write them in one swoop.
			err := db.PutRange(keyvalues)
			if err != nil {
				fmt.Printf("Unable to write slice blocks: %s\n", err.Error())
			}
		}

	}()
	return nil
}

type OpBounds struct {
	blockBeg dvid.Point
	dataBeg  dvid.Point
	dataEnd  dvid.Point
}

func ComputeTransform(v ExtHandler, block *Block, blockSize dvid.Point) (*OpBounds, error) {
	ptIndex, err := datastore.KeyToPointIndexer(block.K)
	if err != nil {
		return nil, err
	}

	// Get the bounding voxel coordinates for this block.
	minBlockVoxel := ptIndex.FirstPoint(blockSize)
	maxBlockVoxel := ptIndex.LastPoint(blockSize)

	// Compute the boundary voxel coordinates for the ExtHandler and adjust
	// to our block bounds.
	minDataVoxel := v.StartPoint()
	maxDataVoxel := v.EndPoint()
	begVolCoord, _ := minDataVoxel.Max(minBlockVoxel)
	endVolCoord, _ := maxDataVoxel.Min(maxBlockVoxel)

	// Adjust the DVID volume voxel coordinates for the data so that (0,0,0)
	// is where we expect this slice/subvolume's data to begin.
	dataBeg := begVolCoord.Sub(v.StartPoint())
	dataEnd := endVolCoord.Sub(v.StartPoint())

	// Compute block coord matching dataBeg
	blockBeg := begVolCoord.Sub(minBlockVoxel)

	// Get the bytes per Voxel
	return &OpBounds{
		blockBeg: blockBeg,
		dataBeg:  dataBeg,
		dataEnd:  dataEnd,
	}, nil
}

func ReadFromBlock(v ExtHandler, block *Block, blockSize dvid.Point) error {
	return transferBlock(GetOp, v, block, blockSize)
}

func WriteToBlock(v ExtHandler, block *Block, blockSize dvid.Point) error {
	return transferBlock(PutOp, v, block, blockSize)
}

func transferBlock(op OpType, v ExtHandler, block *Block, blockSize dvid.Point) error {
	if blockSize.NumDims() > 3 {
		return fmt.Errorf("DVID voxel blocks currently only supports up to 3d, not 4+ dimensions")
	}
	opBounds, err := ComputeTransform(v, block, blockSize)
	if err != nil {
		return err
	}
	data := v.Data()
	bytesPerVoxel := v.Values().BytesPerVoxel()
	blockBeg := opBounds.blockBeg
	dataBeg := opBounds.dataBeg
	dataEnd := opBounds.dataEnd

	// Compute the strides (in bytes)
	bX := blockSize.Value(0) * bytesPerVoxel
	bY := blockSize.Value(1) * bX
	dX := v.Stride()

	// Do the transfers depending on shape of the external voxels.
	switch {
	case v.DataShape().Equals(dvid.XY):
		blockI := blockBeg.Value(2)*bY + blockBeg.Value(1)*bX + blockBeg.Value(0)*bytesPerVoxel
		dataI := dataBeg.Value(1)*dX + dataBeg.Value(0)*bytesPerVoxel
		bytes := (dataEnd.Value(0) - dataBeg.Value(0) + 1) * bytesPerVoxel
		switch op {
		case GetOp:
			for y := dataBeg.Value(1); y <= dataEnd.Value(1); y++ {
				copy(data[dataI:dataI+bytes], block.V[blockI:blockI+bytes])
				blockI += bX
				dataI += dX
			}
		case PutOp:
			for y := dataBeg.Value(1); y <= dataEnd.Value(1); y++ {
				copy(block.V[blockI:blockI+bytes], data[dataI:dataI+bytes])
				blockI += bX
				dataI += dX
			}
		}

	case v.DataShape().Equals(dvid.XZ):
		blockI := blockBeg.Value(2)*bY + blockBeg.Value(1)*bX + blockBeg.Value(0)*bytesPerVoxel
		dataI := dataBeg.Value(2)*v.Stride() + dataBeg.Value(0)*bytesPerVoxel
		bytes := (dataEnd.Value(0) - dataBeg.Value(0) + 1) * bytesPerVoxel
		switch op {
		case GetOp:
			for y := dataBeg.Value(2); y <= dataEnd.Value(2); y++ {
				copy(data[dataI:dataI+bytes], block.V[blockI:blockI+bytes])
				blockI += bY
				dataI += dX
			}
		case PutOp:
			for y := dataBeg.Value(2); y <= dataEnd.Value(2); y++ {
				copy(block.V[blockI:blockI+bytes], data[dataI:dataI+bytes])
				blockI += bY
				dataI += dX
			}
		}

	case v.DataShape().Equals(dvid.YZ):
		bz := blockBeg.Value(2)
		switch op {
		case GetOp:
			for y := dataBeg.Value(2); y <= dataEnd.Value(2); y++ {
				blockI := bz*bY + blockBeg.Value(1)*bX + blockBeg.Value(0)*bytesPerVoxel
				dataI := y*dX + dataBeg.Value(1)*bytesPerVoxel
				for x := dataBeg.Value(1); x <= dataEnd.Value(1); x++ {
					copy(data[dataI:dataI+bytesPerVoxel], block.V[blockI:blockI+bytesPerVoxel])
					blockI += bX
					dataI += bytesPerVoxel
				}
				bz++
			}
		case PutOp:
			for y := dataBeg.Value(2); y <= dataEnd.Value(2); y++ {
				blockI := bz*bY + blockBeg.Value(1)*bX + blockBeg.Value(0)*bytesPerVoxel
				dataI := y*dX + dataBeg.Value(1)*bytesPerVoxel
				for x := dataBeg.Value(1); x <= dataEnd.Value(1); x++ {
					copy(block.V[blockI:blockI+bytesPerVoxel], data[dataI:dataI+bytesPerVoxel])
					blockI += bX
					dataI += bytesPerVoxel
				}
				bz++
			}
		}

	case v.DataShape().ShapeDimensions() == 2:
		// TODO: General code for handling 2d ExtHandler in n-d space.
		return fmt.Errorf("DVID currently does not support 2d in n-d space.")

	case v.DataShape().Equals(dvid.Vol3d):
		blockOffset := blockBeg.Value(0) * bytesPerVoxel
		dX = v.Size().Value(0) * bytesPerVoxel
		dY := v.Size().Value(1) * dX
		dataOffset := dataBeg.Value(0) * bytesPerVoxel
		bytes := (dataEnd.Value(0) - dataBeg.Value(0) + 1) * bytesPerVoxel
		blockZ := blockBeg.Value(2)

		switch op {
		case GetOp:
			for dataZ := dataBeg.Value(2); dataZ <= dataEnd.Value(2); dataZ++ {
				blockY := blockBeg.Value(1)
				for dataY := dataBeg.Value(1); dataY <= dataEnd.Value(1); dataY++ {
					blockI := blockZ*bY + blockY*bX + blockOffset
					dataI := dataZ*dY + dataY*dX + dataOffset
					copy(block.V[blockI:blockI+bytes], data[dataI:dataI+bytes])
					blockY++
				}
				blockZ++
			}
		case PutOp:
			for dataZ := dataBeg.Value(2); dataZ <= dataEnd.Value(2); dataZ++ {
				blockY := blockBeg.Value(1)
				for dataY := dataBeg.Value(1); dataY <= dataEnd.Value(1); dataY++ {
					blockI := blockZ*bY + blockY*bX + blockOffset
					dataI := dataZ*dY + dataY*dX + dataOffset
					copy(data[dataI:dataI+bytes], block.V[blockI:blockI+bytes])
					blockY++
				}
				blockZ++
			}
		}

	default:
		return fmt.Errorf("Cannot ReadFromBlock() unsupported voxels data shape %s", v.DataShape())
	}
	return nil
}

// -------  ExtHandler interface implementation -------------

// Voxels represents subvolumes or slices.
type Voxels struct {
	dvid.Geometry

	values DataValues

	// The data itself
	data []byte

	// The stride for 2d iteration in bytes between vertically adjacent pixels.
	// For 3d subvolumes, we don't reuse standard Go images but maintain fully
	// packed data slices, so stride isn't necessary.
	stride int32

	byteOrder binary.ByteOrder
}

func NewVoxels(geom dvid.Geometry, values DataValues, data []byte, stride int32,
	byteOrder binary.ByteOrder) *Voxels {

	return &Voxels{geom, values, data, stride, byteOrder}
}

func (v *Voxels) String() string {
	size := v.Size()
	return fmt.Sprintf("%s of size %s @ %s", v.DataShape(), size, v.StartPoint())
}

func (v *Voxels) Values() DataValues {
	return v.values
}

func (v *Voxels) Data() []byte {
	return v.data
}

func (v *Voxels) Stride() int32 {
	return v.stride
}

func (v *Voxels) BytesPerVoxel() int32 {
	return v.values.BytesPerVoxel()
}

func (v *Voxels) ByteOrder() binary.ByteOrder {
	return v.byteOrder
}

func (v *Voxels) Index(c dvid.ChunkPoint) dvid.Index {
	return dvid.IndexZYX(c.(dvid.ChunkPoint3d))
}

// IndexIterator returns an iterator that can move across the voxel geometry,
// generating indices or index spans.
func (v *Voxels) IndexIterator(chunkSize dvid.Point) (dvid.IndexIterator, error) {
	// Setup traversal
	begVoxel, ok := v.StartPoint().(dvid.Chunkable)
	if !ok {
		return nil, fmt.Errorf("ExtHandler StartPoint() cannot handle Chunkable points.")
	}
	endVoxel, ok := v.EndPoint().(dvid.Chunkable)
	if !ok {
		return nil, fmt.Errorf("ExtHandler EndPoint() cannot handle Chunkable points.")
	}
	begBlock := begVoxel.Chunk(chunkSize).(dvid.ChunkPoint3d)
	endBlock := endVoxel.Chunk(chunkSize).(dvid.ChunkPoint3d)

	return dvid.NewIndexZYXIterator(v.Geometry, begBlock, endBlock), nil
}

// GoImage returns an image.Image suitable for use external to DVID.
// TODO -- Create more comprehensive handling of endianness and encoding of
// multibytes/voxel data into appropriate images.
func (v *Voxels) GoImage() (img image.Image, err error) {
	// Make sure each value has same # of bytes or else we can't generate a go image.
	// If so, we need to make another ExtHandler that knows how to convert the varying
	// values into an appropriate go image.
	valuesPerVoxel := int32(len(v.values))
	if valuesPerVoxel < 1 || valuesPerVoxel > 4 {
		return nil, fmt.Errorf("Standard voxels type can't convert %d values/voxel into go image.", valuesPerVoxel)
	}
	bytesPerValue := v.values.ValueBytes(0)
	for _, dataValue := range v.values {
		if typeBytes[dataValue.DataType] != bytesPerValue {
			return nil, fmt.Errorf("Standard voxels type can't handle varying sized values per voxel.")
		}
	}

	unsupported := func() error {
		return fmt.Errorf("DVID doesn't support images for %d channels and %d bytes/channel",
			valuesPerVoxel, bytesPerValue)
	}

	width := v.Size().Value(0)
	height := v.Size().Value(1)
	sliceBytes := width * height * valuesPerVoxel * bytesPerValue
	beg := int32(0)
	end := beg + sliceBytes
	data := v.Data()
	if int(end) > len(data) {
		err = fmt.Errorf("Voxels %s has insufficient amount of data to return an image.", v)
		return
	}
	r := image.Rect(0, 0, int(width), int(height))
	switch valuesPerVoxel {
	case 1:
		switch bytesPerValue {
		case 1:
			img = &image.Gray{data[beg:end], 1 * r.Dx(), r}
		case 2:
			bigendian, err := littleToBigEndian(v, data[beg:end])
			if err != nil {
				return nil, err
			}
			img = &image.Gray16{bigendian, 2 * r.Dx(), r}
		case 4:
			img = &image.RGBA{data[beg:end], 4 * r.Dx(), r}
		case 8:
			img = &image.RGBA64{data[beg:end], 8 * r.Dx(), r}
		default:
			err = unsupported()
		}
	case 4:
		switch bytesPerValue {
		case 1:
			// HACK -- This should be handled client side in shader.
			for i := int32(0); i < sliceBytes; i += 4 {
				v := float32(data[i+3]) / 255.0
				data[i] = uint8(float32(data[i]) * v)
				data[i+1] = uint8(float32(data[i+1]) * v)
				data[i+2] = uint8(float32(data[i+2]) * v)
				data[i+3] = 255
			}
			img = &image.RGBA{data[beg:end], 4 * r.Dx(), r}
		case 2:
			img = &image.RGBA64{data[beg:end], 8 * r.Dx(), r}
		default:
			err = unsupported()
		}
	default:
		err = unsupported()
	}
	return
}

// Datatype embeds the datastore's Datatype to create a unique type
// with voxel functions.  Refinements of general voxel types can be implemented
// by embedding this type, choosing appropriate # of values and bytes/value,
// overriding functions as needed, and calling datastore.RegisterDatatype().
// Note that these fields are invariant for all instances of this type.  Fields
// that can change depending on the type of data (e.g., resolution) should be
// in the Data type.
type Datatype struct {
	datastore.Datatype

	// values describes the data type/label for each value within a voxel.
	values DataValues
}

// NewDatatype returns a pointer to a new voxels Datatype with default values set.
func NewDatatype(values DataValues) (dtype *Datatype) {
	dtype = new(Datatype)
	dtype.values = values

	dtype.Requirements = &storage.Requirements{
		BulkIniter: false,
		BulkWriter: false,
		Batcher:    true,
	}
	return
}

// --- TypeService interface ---

// NewData returns a pointer to a new Voxels with default values.
func (dtype *Datatype) NewDataService(id *datastore.DataID, config dvid.Config) (
	service datastore.DataService, err error) {

	var basedata *datastore.Data
	basedata, err = datastore.NewDataService(id, dtype, config)
	if err != nil {
		return
	}
	props := Properties{
		Values: make([]DataValue, len(dtype.values)),
	}
	copy(props.Values, dtype.values)

	var dimensions int
	var s string
	var found bool
	dimensions, found, err = config.GetInt("Dimensions")
	if err != nil {
		return
	}
	if !found {
		dimensions = 3
	}
	s, found, err = config.GetString("BlockSize")
	if err != nil {
		return
	}
	if found {
		props.BlockSize, err = dvid.StringToPoint(s, ",")
		if err != nil {
			return
		}
	} else {
		size := make([]int32, dimensions)
		for d := 0; d < dimensions; d++ {
			size[d] = DefaultBlockSize
		}
		props.BlockSize, err = dvid.NewPoint(size)
		if err != nil {
			return
		}
	}
	s, found, err = config.GetString("Res")
	if err != nil {
		return
	}
	if found {
		props.Resolution.VoxelSize, err = dvid.StringToNdFloat32(s, ",")
		if err != nil {
			return
		}
	} else {
		props.Resolution.VoxelSize = make(dvid.NdFloat32, dimensions)
		for d := 0; d < dimensions; d++ {
			props.Resolution.VoxelSize[d] = DefaultRes
		}
	}
	s, found, err = config.GetString("Units")
	if err != nil {
		return
	}
	if found {
		props.Resolution.VoxelUnits, err = dvid.StringToNdString(s, ",")
		if err != nil {
			return
		}
	} else {
		props.Resolution.VoxelUnits = make(dvid.NdString, dimensions)
		for d := 0; d < dimensions; d++ {
			props.Resolution.VoxelUnits[d] = DefaultUnits
		}
	}
	service = &Data{
		Data:       *basedata,
		Properties: props,
	}
	return
}

func (dtype *Datatype) Help() string {
	return fmt.Sprintf(HelpMessage, DefaultBlockSize)
}

// DataValue describes the data type and label for each value within a voxel.
type DataValue struct {
	DataType string
	Label    string
}

// DataValues describes the interleaved values within a voxel.
type DataValues []DataValue

var typeBytes = map[string]int32{
	"uint8":   1,
	"int8":    1,
	"uint16":  2,
	"int16":   2,
	"uint32":  4,
	"int32":   4,
	"uint64":  8,
	"int64":   8,
	"float32": 4,
	"float64": 8,
}

func (values DataValues) BytesPerVoxel() int32 {
	var bytes int32
	for _, dataValue := range values {
		bytes += typeBytes[dataValue.DataType]
	}
	return bytes
}

func (values DataValues) ValueBytes(dim int) int32 {
	if dim < 0 || dim >= len(values) {
		return 0
	}
	return typeBytes[values[dim].DataType]
}

// Extents holds the extents of a volume in both absolute voxel coordinates
// and lexicographically sorted chunk indices.
type Extents struct {
	MinPoint dvid.Point
	MaxPoint dvid.Point

	MinIndex dvid.PointIndexer
	MaxIndex dvid.PointIndexer

	pointMu sync.Mutex
	indexMu sync.Mutex
}

// AdjustPoints modifies extents based on new voxel coordinates in concurrency-safe manner.
func (ext *Extents) AdjustPoints(pointBeg, pointEnd dvid.Point) bool {
	ext.pointMu.Lock()
	defer ext.pointMu.Unlock()

	var changed bool
	if ext.MinPoint == nil {
		ext.MinPoint = pointBeg
		changed = true
	} else {
		ext.MinPoint, changed = ext.MinPoint.Min(pointBeg)
	}
	if ext.MaxPoint == nil {
		ext.MaxPoint = pointEnd
		changed = true
	} else {
		ext.MaxPoint, changed = ext.MaxPoint.Max(pointEnd)
	}
	return changed
}

// AdjustIndices modifies extents based on new block indices in concurrency-safe manner.
func (ext *Extents) AdjustIndices(indexBeg, indexEnd dvid.PointIndexer) bool {
	ext.indexMu.Lock()
	defer ext.indexMu.Unlock()

	var changed bool
	if ext.MinIndex == nil {
		ext.MinIndex = indexBeg
		changed = true
	} else {
		ext.MinIndex, changed = ext.MinIndex.Min(indexBeg)
	}
	if ext.MaxIndex == nil {
		ext.MaxIndex = indexEnd
		changed = true
	} else {
		ext.MaxIndex, changed = ext.MaxIndex.Max(indexEnd)
	}
	return changed
}

type Resolution struct {
	// Resolution of voxels in volume
	VoxelSize dvid.NdFloat32

	// Units of resolution, e.g., "nanometers"
	VoxelUnits dvid.NdString
}

type Properties struct {
	// Values describes the data type/label for each value within a voxel.
	Values DataValues

	// Block size for this dataset
	BlockSize dvid.Point

	// The endianness of this loaded data.
	ByteOrder binary.ByteOrder

	Resolution
	Extents
}

type dataSchema struct {
	Axes   []axis
	Values DataValues
}

type axis struct {
	Label      string
	Resolution float32
	Units      string
	Size       int32
	Offset     int32
}

// TODO -- Allow explicit setting of axes labels.
var axesName = []string{"X", "Y", "Z", "t", "c"}

// NdDataSchema returns the JSON schema for this Data
func (props *Properties) NdDataSchema() (string, error) {
	var err error
	var size, offset dvid.Point

	dims := int(props.BlockSize.NumDims())
	if props.MinPoint == nil || props.MaxPoint == nil {
		zeroPt := make([]int32, dims)
		size, err = dvid.NewPoint(zeroPt)
		if err != nil {
			return "", err
		}
		offset = size
	} else {
		size = props.MaxPoint.Sub(props.MinPoint).AddScalar(1)
		offset = props.MinPoint
	}

	var schema dataSchema
	schema.Axes = []axis{}
	for dim := 0; dim < dims; dim++ {
		schema.Axes = append(schema.Axes, axis{
			Label:      axesName[dim],
			Resolution: props.Resolution.VoxelSize[dim],
			Units:      props.Resolution.VoxelUnits[dim],
			Size:       size.Value(uint8(dim)),
			Offset:     offset.Value(uint8(dim)),
		})
	}
	schema.Values = props.Values

	m, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}
	return string(m), nil
}

// Data embeds the datastore's Data and extends it with voxel-specific properties.
type Data struct {
	datastore.Data
	Properties
}

// ----- IntHandler interface implementation ----------

// NewExtHandler returns an ExtHandler given some geometry and optional image data.
// If img is passed in, the function will initialize the ExtHandler with data from the image.
// Otherwise, it will allocate a zero buffer of appropriate size.
func (d *Data) NewExtHandler(geom dvid.Geometry, img interface{}) (ExtHandler, error) {
	bytesPerVoxel := d.Properties.Values.BytesPerVoxel()
	stride := geom.Size().Value(0) * bytesPerVoxel

	voxels := &Voxels{
		Geometry:  geom,
		values:    d.Properties.Values,
		stride:    stride,
		byteOrder: d.ByteOrder,
	}

	if img == nil {
		numVoxels := geom.NumVoxels()
		if numVoxels <= 0 {
			return nil, fmt.Errorf("Illegal geometry requested: %s", geom)
		}
		if numVoxels > MaxVoxelsRequest {
			return nil, fmt.Errorf("Requested # voxels (%d) exceeds this DVID server's set limit (%d)",
				geom.NumVoxels(), MaxVoxelsRequest)
		}
		voxels.data = make([]uint8, int64(bytesPerVoxel)*geom.NumVoxels())
	} else {
		switch t := img.(type) {
		case image.Image:
			var actualStride int32
			var err error
			voxels.data, _, actualStride, err = dvid.ImageData(t)
			if err != nil {
				return nil, err
			}
			if actualStride < stride {
				return nil, fmt.Errorf("Too little data in input image (expected stride %d)", stride)
			}
			voxels.stride = actualStride
		default:
			return nil, fmt.Errorf("Unexpected image type given to NewExtHandler(): %T", t)
		}
	}
	return voxels, nil
}

func (d *Data) DataID() datastore.DataID {
	return *(d.Data.DataID)
}

func (d *Data) Values() DataValues {
	return d.Properties.Values
}

func (d *Data) BlockSize() dvid.Point {
	return d.Properties.BlockSize
}

func (d *Data) Extents() *Extents {
	return &(d.Properties.Extents)
}

func (d *Data) String() string {
	return string(d.DataName())
}

// JSONString returns the JSON for this Data's configuration
func (d *Data) JSONString() (jsonStr string, err error) {
	m, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(m), nil
}

// --- DataService interface ---

// DoRPC acts as a switchboard for RPC commands.
func (d *Data) DoRPC(request datastore.Request, reply *datastore.Response) error {
	switch request.TypeCommand() {
	case "load":
		if len(request.Command) < 5 {
			return fmt.Errorf("Poorly formatted load command.  See command-line help.")
		}
		// Parse the request
		var uuidStr, dataName, cmdStr, offsetStr string
		filenames, err := request.FilenameArgs(1, &uuidStr, &dataName, &cmdStr, &offsetStr)
		if err != nil {
			return err
		}
		if len(filenames) == 0 {
			return fmt.Errorf("Need to include at least one file to add: %s", request)
		}

		// Get offset
		offset, err := dvid.StringToPoint(offsetStr, ",")
		if err != nil {
			return fmt.Errorf("Illegal offset specification: %s: %s", offsetStr, err.Error())
		}

		// Get list of files to add
		var addedFiles string
		if len(filenames) == 1 {
			addedFiles = filenames[0]
		} else {
			addedFiles = fmt.Sprintf("filenames: %s [%d more]", filenames[0], len(filenames)-1)
		}
		dvid.Log(dvid.Debug, addedFiles+"\n")

		// Get version node
		uuid, err := server.MatchingUUID(uuidStr)
		if err != nil {
			return err
		}

		return LoadXY(d, uuid, offset, filenames)

	case "put":
		if len(request.Command) < 7 {
			return fmt.Errorf("Poorly formatted put command.  See command-line help.")
		}
		source := request.Command[4]
		switch source {
		case "local":
			return d.PutLocal(request, reply)
		case "remote":
			return fmt.Errorf("put remote not yet implemented")
		default:
			return d.UnknownCommand(request)
		}

	default:
		return d.UnknownCommand(request)
	}
	return nil
}

// DoHTTP handles all incoming HTTP requests for this data.
func (d *Data) DoHTTP(uuid dvid.UUID, w http.ResponseWriter, r *http.Request) error {
	startTime := time.Now()

	// Allow cross-origin resource sharing.
	w.Header().Add("Access-Control-Allow-Origin", "*")

	// Get the action (GET, POST)
	action := strings.ToLower(r.Method)
	var op OpType
	switch action {
	case "get":
		op = GetOp
	case "post":
		op = PutOp
	default:
		return fmt.Errorf("Can only handle GET or POST HTTP verbs")
	}

	// Break URL request into arguments
	url := r.URL.Path[len(server.WebAPIPath):]
	parts := strings.Split(url, "/")

	// Process help and info.
	switch parts[3] {
	case "help":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, d.Help())
		return nil
	case "schema":
		jsonStr, err := d.NdDataSchema()
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return err
		}
		w.Header().Set("Content-Type", "application/vnd.dvid-nd-data+json")
		fmt.Fprintln(w, jsonStr)
		return nil
	case "info":
		jsonStr, err := d.JSONString()
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, jsonStr)
		return nil
	default:
	}

	// Get the data shape.
	shapeStr := dvid.DataShapeString(parts[3])
	dataShape, err := shapeStr.DataShape()
	if err != nil {
		return fmt.Errorf("Bad data shape given '%s'", shapeStr)
	}

	switch dataShape.ShapeDimensions() {
	case 2:
		sizeStr, offsetStr := parts[4], parts[5]
		slice, err := dvid.NewSliceFromStrings(shapeStr, offsetStr, sizeStr, "_")
		if err != nil {
			return err
		}
		if op == PutOp {
			// TODO -- Put in format checks for POSTed image.
			postedImg, _, err := dvid.ImageFromPost(r, "image")
			if err != nil {
				return err
			}
			e, err := d.NewExtHandler(slice, postedImg)
			if err != nil {
				return err
			}
			err = PutImage(uuid, d, e)
			if err != nil {
				return err
			}
		} else {
			e, err := d.NewExtHandler(slice, nil)
			if err != nil {
				return err
			}
			img, err := GetImage(uuid, d, e)
			if err != nil {
				return err
			}
			var formatStr string
			if len(parts) >= 7 {
				formatStr = parts[6]
			}
			//dvid.ElapsedTime(dvid.Normal, startTime, "%s %s upto image formatting", op, slice)
			err = dvid.WriteImageHttp(w, img, formatStr)
			if err != nil {
				return err
			}
		}
	case 3:
		sizeStr, offsetStr := parts[4], parts[5]
		subvol, err := dvid.NewSubvolumeFromStrings(offsetStr, sizeStr, "_")
		if err != nil {
			return err
		}
		if op == GetOp {
			e, err := d.NewExtHandler(subvol, nil)
			if err != nil {
				return err
			}
			if data, err := GetVolume(uuid, d, e); err != nil {
				return err
			} else {
				w.Header().Set("Content-type", "application/octet-stream")
				_, err = w.Write(data)
				if err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("DVID does not yet support POST of volume data")
		}
	default:
		return fmt.Errorf("DVID currently supports shapes of only 2 and 3 dimensions")
	}

	dvid.ElapsedTime(dvid.Debug, startTime, "HTTP %s: %s (%s)", r.Method, dataShape, r.URL)
	return nil
}

// ProcessChunk processes a chunk of data as part of a mapped operation.  The data may be
// thinner, wider, and longer than the chunk, depending on the data shape (XY, XZ, etc).
// Only some multiple of the # of CPU cores can be used for chunk handling before
// it waits for chunk processing to abate via the buffered server.HandlerToken channel.
func (d *Data) ProcessChunk(chunk *storage.Chunk) {
	<-server.HandlerToken
	go d.processChunk(chunk)
}

func (d *Data) processChunk(chunk *storage.Chunk) {
	defer func() {
		// After processing a chunk, return the token.
		server.HandlerToken <- 1

		// Notify the requestor that this chunk is done.
		if chunk.Wg != nil {
			chunk.Wg.Done()
		}
	}()

	op, ok := chunk.Op.(*Operation)
	if !ok {
		log.Fatalf("Illegal operation passed to ProcessChunk() for data %s\n", d.DataName())
	}

	// Initialize the block buffer using the chunk of data.  For voxels, this chunk of
	// data needs to be uncompressed and deserialized.
	var err error
	var blockData []byte
	if chunk == nil || chunk.V == nil {
		blockData = make([]byte, d.BlockSize().Prod()*int64(op.Values().BytesPerVoxel()))
	} else {
		blockData, _, err = dvid.DeserializeData(chunk.V, true)
		if err != nil {
			dvid.Log(dvid.Normal, "Unable to deserialize block in '%s': %s\n",
				d.DataID().DataName(), err.Error())
		}
	}

	// Perform the operation.
	block := &Block{K: chunk.K, V: blockData}
	switch op.OpType {
	case GetOp:
		if err = ReadFromBlock(op.ExtHandler, block, d.BlockSize()); err != nil {
			log.Fatalln(err.Error())
		}
	case PutOp:
		if err = WriteToBlock(op.ExtHandler, block, d.BlockSize()); err != nil {
			log.Fatalln(err.Error())
		}
		db := server.StorageEngine()
		serialization, err := dvid.SerializeData(blockData, dvid.Snappy, dvid.CRC32)
		if err != nil {
			fmt.Printf("Unable to serialize block: %s\n", err.Error())
		}
		db.Put(chunk.K, serialization)
	}
}

// Handler conversion of little to big endian for voxels larger than 1 byte.
func littleToBigEndian(v ExtHandler, data []uint8) (bigendian []uint8, err error) {
	bytesPerVoxel := v.Values().BytesPerVoxel()
	if v.ByteOrder() == nil || v.ByteOrder() == binary.BigEndian || bytesPerVoxel == 1 {
		return data, nil
	}
	bigendian = make([]uint8, len(data))
	switch bytesPerVoxel {
	case 2:
		for beg := 0; beg < len(data)-1; beg += 2 {
			bigendian[beg], bigendian[beg+1] = data[beg+1], data[beg]
		}
	case 4:
		for beg := 0; beg < len(data)-3; beg += 4 {
			value := binary.LittleEndian.Uint32(data[beg : beg+4])
			binary.BigEndian.PutUint32(bigendian[beg:beg+4], value)
		}
	case 8:
		for beg := 0; beg < len(data)-7; beg += 8 {
			value := binary.LittleEndian.Uint64(data[beg : beg+8])
			binary.BigEndian.PutUint64(bigendian[beg:beg+8], value)
		}
	}
	return
}
