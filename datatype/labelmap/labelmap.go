/*
	Package labelmap implements DVID support for label->label mapping including
	spatial index tracking.
*/
package labelmap

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/labels64"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version = "0.1"
	RepoUrl = "github.com/janelia-flyem/dvid/datatype/labelmap"
)

const HelpMessage = `
API for 'labelmap' datatype (github.com/janelia-flyem/dvid/datatype/labelmap)
=============================================================================

Command-line:

$ dvid dataset <UUID> new labelmap <data name> <settings...>

	Adds newly named labelmap data to dataset with specified UUID.

	Example:

	$ dvid dataset 3f8c new labelmap sp2body Labels=mylabels

    Arguments:

    UUID             Hexidecimal string with enough characters to uniquely identify a version node.
    data name        Name of data to create, e.g., "sp2body"
    settings         Configuration settings in "key=value" format separated by spaces.

    Configuration Settings (case-insensitive keys)

    Labels           Name of labels64 data for which this is a label mapping. (required)
    Versioned        "true" or "false" (default)

$ dvid node <UUID> <data name> load raveler <superpixel-to-segment filename> <segment-to-body filename>

    Loads a superpixel-to-body mapping using two Raveler-formatted text files.

    Example: 

    $ dvid node 3f8c sp2body load raveler superpixel_to_segment_map.txt segment_to_body_map.txt

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
	
	
    ------------------

HTTP API (Level 2 REST):

Note that browsers support HTTP PUT and DELETE via javascript but only GET/POST are
included in HTML specs.  For ease of use in constructing clients, HTTP POST is used
to create or modify resources in an idempotent fashion.

GET  /api/node/<UUID>/<data name>/help

	Returns data-specific help message.


GET  /api/node/<UUID>/<data name>/info
POST /api/node/<UUID>/<data name>/info

    Retrieves or puts data properties.

    Example: 

    GET /api/node/3f8c/stuff/info

    Returns JSON with configuration settings.

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of mapping data.


`

func init() {
	labelmap := NewDatatype()
	labelmap.DatatypeID = &datastore.DatatypeID{
		Name:    "labelmap",
		Url:     RepoUrl,
		Version: Version,
	}
	datastore.RegisterDatatype(labelmap)

	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Datatype{})
	gob.Register(&Data{})
	gob.Register(&binary.LittleEndian)
	gob.Register(&binary.BigEndian)
}

var (
	emptyValue          = []byte{}
	zeroSuperpixelBytes = make([]byte, 8, 8)
)

type KeyType byte

const (
	// KeyInverseMap are keys that have label2 + spatial index + label1.
	// For superpixel->body maps, this key would be body-block-superpixel.
	KeyInverseMap KeyType = iota

	// KeyForwardMap are keys for label1 -> label2 maps, so the keys are label1.
	// For superpixel->body maps, this key would be the superpixel label.
	KeyForwardMap

	// KeySpatialMap are keys composed of spatial index + label + forward label.
	// They are useful for composing label maps for a spatial index.
	KeySpatialMap

	// KeyLabelSpatialMap are keys for forward label -> spatial indices where the
	// spatial indices are blocks that have labels that map to the forward label.
	// They are useful for returning all blocks intersected by a label.
	KeyLabelSpatialMap
)

func (t KeyType) String() string {
	switch t {
	case KeyInverseMap:
		return "Inverse Label Map"
	case KeyForwardMap:
		return "Forward Label Map"
	case KeySpatialMap:
		return "Spatial Index to Labels Map"
	default:
		return "Unknown Key Type"
	}
}

type Operation struct {
	labels    *labels64.Data
	versionID dvid.VersionLocalID
}

func getRelatedLabels(uuid dvid.UUID, name dvid.DataString) (*labels64.Data, error) {
	service := server.DatastoreService()
	source, err := service.DataService(uuid, name)
	if err != nil {
		return nil, err
	}
	data, ok := source.(*labels64.Data)
	if !ok {
		return nil, fmt.Errorf("Can only use labelmap with labels64 data: %s", name)
	}
	return data, nil
}

// Datatype embeds the datastore's Datatype to create a unique type for labelmap functions.
type Datatype struct {
	datastore.Datatype
}

// NewDatatype returns a pointer to a new labelmap Datatype with default values set.
func NewDatatype() (dtype *Datatype) {
	dtype = new(Datatype)
	dtype.Requirements = &storage.Requirements{
		BulkIniter: false,
		BulkWriter: false,
		Batcher:    true,
	}
	return
}

// --- TypeService interface ---

// NewData returns a pointer to new labelmap data with default values.
func (dtype *Datatype) NewDataService(id *datastore.DataID, c dvid.Config) (datastore.DataService, error) {
	// Make sure we have valid labels64 data for mapping
	name, found, err := c.GetString("Labels")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("Cannot make labelmap without valid 'Labels' setting.")
	}
	labelsName := dvid.DataString(name)

	basedata, err := datastore.NewDataService(id, dtype, c)
	if err != nil {
		return nil, err
	}
	return &Data{Data: basedata, Labels: labelsName}, nil
}

func (dtype *Datatype) Help() string {
	return fmt.Sprintf(HelpMessage)
}

// Data embeds the datastore's Data and extends it with keyvalue properties (none for now).
type Data struct {
	*datastore.Data

	// Labels64 data that we will be mapping.
	Labels dvid.DataString

	// ZeroLocked is true if the zero label is locked and always mapped to zero.
	ZeroLocked bool

	// Ready is true if inverse map, forward map, and spatial queries are ready.
	Ready bool

	// private counter of chunk processing for status messages
	processingZ      int32
	processingZMutex sync.RWMutex
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
		if len(request.Command) < 6 {
			return fmt.Errorf("Poorly formatted load command.  See command-line help.")
		}
		switch request.Command[4] {
		case "raveler":
			return d.LoadRavelerMaps(request, reply)
		default:
			return fmt.Errorf("Cannot load unknown input file types '%s'", request.Command[3])
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

	// Break URL request into arguments
	url := r.URL.Path[len(server.WebAPIPath):]
	parts := strings.Split(url, "/")

	// Process help and info.
	switch parts[3] {
	case "help":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, d.Help())
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

	// Get the key and process request
	var comment string
	switch strings.ToLower(r.Method) {
	case "get":
	case "post":
	default:
		return fmt.Errorf("Can only handle GET or POST HTTP verbs")
	}

	dvid.ElapsedTime(dvid.Debug, startTime, comment)
	return nil
}

func loadSegBodyMap(filename string) (map[uint64]uint64, error) {
	startTime := time.Now()
	dvid.Log(dvid.Normal, "Loading segment->body map: %s\n", filename)

	segmentToBodyMap := make(map[uint64]uint64, 100000)
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("Could not open segment->body map: %s", filename)
	}
	defer file.Close()
	linenum := 0
	lineReader := bufio.NewReader(file)
	for {
		line, err := lineReader.ReadString('\n')
		if err != nil {
			break
		}
		if line[0] == ' ' || line[0] == '#' {
			continue
		}
		storage.FileBytesRead <- len(line)
		var segment, body uint64
		if _, err := fmt.Sscanf(line, "%d %d", &segment, &body); err != nil {
			return nil, fmt.Errorf("Error loading segment->body map, line %d in %s", linenum, filename)
		}
		segmentToBodyMap[segment] = body
		linenum++
	}
	dvid.ElapsedTime(dvid.Debug, startTime, "Loaded Raveler segment->body file: %s", filename)
	return segmentToBodyMap, nil
}

// LoadRavelerMaps loads maps from Raveler-formatted superpixel->segment and
// segment->body maps.
func (d *Data) LoadRavelerMaps(request datastore.Request, reply *datastore.Response) error {
	// Use of Raveler maps causes zero labels to be reserved.
	d.ZeroLocked = true

	// Parse the request
	var uuidStr, dataName, cmdStr, fileTypeStr, spsegStr, segbodyStr string
	request.CommandArgs(1, &uuidStr, &dataName, &cmdStr, &fileTypeStr, &spsegStr, &segbodyStr)

	// Get the version
	uuid, err := server.MatchingUUID(uuidStr)
	if err != nil {
		return err
	}
	/*
		startTime := time.Now()

		// Get the seg->body map
		seg2body, err := loadSegBodyMap(segbodyStr)
		if err != nil {
			return err
		}

		// Prepare for datastore access
		versionID, err := server.VersionLocalID(uuid)
		if err != nil {
			return err
		}
		db := server.StorageEngine()

		var slice, superpixel32 uint32
		var segment, body uint64
		forwardIndex := make([]byte, 17)
		forwardIndex[0] = byte(KeyForwardMap)
		inverseIndex := make([]byte, 17)
		inverseIndex[0] = byte(KeyInverseMap)

		// Get the sp->seg map, persisting each computed sp->body.
		dvid.Log(dvid.Normal, "Loading and processing superpixel->segment map: %s\n", spsegStr)
		file, err := os.Open(spsegStr)
		if err != nil {
			return fmt.Errorf("Could not open superpixel->segment map: %s", spsegStr)
		}
		defer file.Close()
		lineReader := bufio.NewReader(file)
		linenum := 0

		for {
			line, err := lineReader.ReadString('\n')
			if err != nil {
				break
			}
			if line[0] == ' ' || line[0] == '#' {
				continue
			}
			storage.FileBytesRead <- len(line)
			if _, err := fmt.Sscanf(line, "%d %d %d", &slice, &superpixel32, &segment); err != nil {
				return fmt.Errorf("Error loading superpixel->segment map, line %d in %s", linenum, spsegStr)
			}
			if superpixel32 == 0 {
				continue
			}
			if superpixel32 > 0x0000000000FFFFFF {
				return fmt.Errorf("Error in line %d: superpixel id exceeds 24-bit value!", linenum)
			}
			superpixelBytes := labels64.RavelerSuperpixelBytes(slice, superpixel32)
			var found bool
			body, found = seg2body[segment]
			if !found {
				return fmt.Errorf("Segment (%d) in %s not found in %s", segment, spsegStr, segbodyStr)
			}

			// PUT the forward label pair without compression.
			copy(forwardIndex[1:9], superpixelBytes)
			binary.BigEndian.PutUint64(forwardIndex[9:17], body)
			key := d.DataKey(versionID, dvid.IndexBytes(forwardIndex))
			err = db.Put(key, emptyValue)
			if err != nil {
				return fmt.Errorf("ERROR on PUT of forward label mapping (%x -> %d): %s\n",
					superpixelBytes, body, err.Error())
			}

			// PUT the inverse label pair without compression.
			binary.BigEndian.PutUint64(inverseIndex[1:9], body)
			copy(inverseIndex[9:17], superpixelBytes)
			key = d.DataKey(versionID, dvid.IndexBytes(inverseIndex))
			err = db.Put(key, emptyValue)
			if err != nil {
				return fmt.Errorf("ERROR on PUT of inverse label mapping (%d -> %x): %s\n",
					body, superpixelBytes, err.Error())
			}

			linenum++
			if linenum%1000000 == 0 {
				fmt.Printf("Added %d forward and inverse mappings\n", linenum)
			}
		}
		dvid.Log(dvid.Normal, "Added %d forward and inverse mappings\n", linenum)
		dvid.ElapsedTime(dvid.Normal, startTime, "Processed Raveler superpixel->body files")
	*/
	// Spawn goroutine to do spatial processing on associated label volume.
	go d.ProcessSpatially(uuid)

	return nil
}

// GetLabelMapping returns the mapping for a label.
func (d *Data) GetLabelMapping(versionID dvid.VersionLocalID, label []byte) (uint64, error) {
	firstIndex := make([]byte, 17)
	firstIndex[0] = byte(KeyForwardMap)
	copy(firstIndex[1:9], label)
	binary.BigEndian.PutUint64(firstIndex[9:17], 0)
	lastIndex := make([]byte, 17)
	lastIndex[0] = byte(KeyForwardMap)
	copy(lastIndex[1:9], label)
	binary.BigEndian.PutUint64(lastIndex[9:17], 0xFFFFFFFFFFFFFFFF)

	firstKey := d.DataKey(versionID, dvid.IndexBytes(firstIndex))
	lastKey := d.DataKey(versionID, dvid.IndexBytes(lastIndex))

	db := server.StorageEngine()
	if db == nil {
		return 0, fmt.Errorf("Did not find a working key-value datastore to get image!")
	}
	keys, err := db.KeysInRange(firstKey, lastKey)
	if err != nil {
		return 0, err
	}
	numKeys := len(keys)
	switch {
	case numKeys == 0:
		return 0, fmt.Errorf("Label %d is not mapped to any other label.", label)
	case numKeys > 1:
		var mapped string
		for i := 0; i < len(keys); i++ {
			mapped += fmt.Sprintf("%d ", keys[i])
		}
		return 0, fmt.Errorf("Label %d is mapped to more than one label: %s", label, mapped)
	}

	b := keys[0].Bytes()
	indexBytes := b[datastore.DataKeyIndexOffset:]
	mapping := binary.BigEndian.Uint64(indexBytes[9:17])

	return mapping, nil
}

// Iterate through all blocks in the associated label volume, computing the spatial indices
// for bodies and the mappings for each spatial index.
func (d *Data) ProcessSpatially(uuid dvid.UUID) {
	startTime := time.Now()
	dvid.Log(dvid.Normal, "Adding spatial information from label volume %s for mapping %s...\n",
		d.Labels, d.DataName())

	service := server.DatastoreService()
	_, versionID, err := service.LocalIDFromUUID(uuid)
	if err != nil {
		dvid.Log(dvid.Normal, "Error in getting version ID from UUID '%s': %s\n", uuid, err.Error())
		return
	}

	db := server.StorageEngine()
	if db == nil {
		dvid.Log(dvid.Normal, "Did not find a working key-value datastore to get image!")
		return
	}

	labels, err := getRelatedLabels(uuid, d.Labels)
	if err != nil {
		dvid.Log(dvid.Normal, "Error in getting related labels ('%s'): %s\n", d.Labels, err.Error())
		return
	}

	// Initialize the z counter for status messages.
	extents := labels.Extents()
	d.processingZMutex.Lock()
	d.processingZ = extents.MinIndex.FirstPoint(labels.BlockSize()).Value(2)
	d.processingZMutex.Unlock()

	// Iterate through all indices for the labels data.
	dataID := labels.DataID()
	startKey := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, extents.MinIndex}
	endKey := &datastore.DataKey{dataID.DsetID, dataID.ID, versionID, extents.MaxIndex}

	wg := new(sync.WaitGroup)
	op := &Operation{labels, versionID}
	chunkOp := &storage.ChunkOp{op, wg}

	err = db.ProcessRange(startKey, endKey, chunkOp, d.ProcessChunk)

	// Wait for results then set Updating.
	wg.Wait()
	d.Ready = true

	dvid.ElapsedTime(dvid.Debug, startTime, "Processed spatial information from %s for mapping %s",
		d.Labels, d.DataName())
}

// ProcessChunk processes a chunk of data as part of a mapped operation.
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
	}()

	op := chunk.Op.(*Operation)
	db := server.StorageEngine()
	if db == nil {
		dvid.Log(dvid.Normal, "Did not find a working key-value datastore to get image!")
		return
	}

	// Get the spatial index associated with this chunk.
	dataKey := chunk.K.(*datastore.DataKey)
	zyx := dataKey.Index.(*dvid.IndexZYX)
	zyxBytes := zyx.Bytes()

	// Print status if we are processing a new Z
	firstPt := zyx.FirstPoint(op.labels.BlockSize())
	fmt.Printf("Procesing %s\n", firstPt)
	z := firstPt.Value(2)
	d.processingZMutex.Lock()
	if z > d.processingZ {
		d.processingZ = z
		fmt.Printf("Z = %d: Creating spatial indexes for labels (%s)\n", z, d.DataName())
	}
	d.processingZMutex.Unlock()

	// Initialize the label buffer.  For voxels, this data needs to be uncompressed and deserialized.
	blockData, _, err := dvid.DeserializeData(chunk.V, true)
	if err != nil {
		dvid.Log(dvid.Normal, "Unable to deserialize block in '%s': %s\n",
			d.DataID.DataName(), err.Error())
		return
	}

	// Construct keys that allow quick range queries pertinent to access patterns.
	// We work with the spatial index (s), original label (a), and mapped label (b).
	spatialMapIndex := make([]byte, 1+dvid.IndexZYXSize+8+8) // s + a + b
	spatialMapIndex[0] = byte(KeySpatialMap)
	labelSpatialMapIndex := make([]byte, 1+8+dvid.IndexZYXSize) // b + s
	labelSpatialMapIndex[0] = byte(KeyLabelSpatialMap)

	// Iterate through this block of labels.
	blockBytes := len(blockData)
	if blockBytes%8 != 0 {
		dvid.Log(dvid.Normal, "Retrieved, deserialized block is wrong size: %d bytes\n", blockBytes)
		return
	}

	for start := 0; start < blockBytes; start += 8 {
		a := blockData[start : start+8]

		// If this is zero label and we have locked zero value, ignore.
		if d.ZeroLocked && bytes.Compare(a, zeroSuperpixelBytes) == 0 {
			continue
		}

		// Get the label to which the current label is mapped.
		b, err := d.GetLabelMapping(op.versionID, a)
		if err != nil {
			dvid.Log(dvid.Normal, "Error on getting forward label for %x: %s\n", a, err.Error())
			return
		}

		// Store a KeySpatialMap key (index = s + a + b)
		i := 1 + dvid.IndexZYXSize
		copy(spatialMapIndex[1:i], zyxBytes)
		copy(spatialMapIndex[i:i+8], a)
		binary.BigEndian.PutUint64(spatialMapIndex[i+8:i+16], b)
		key := d.DataKey(op.versionID, dvid.IndexBytes(spatialMapIndex))
		if err = db.Put(key, emptyValue); err != nil {
			dvid.Log(dvid.Normal, "Error on PUT of KeySpatialMap: %s + %x + %d\n", dataKey.Index, a, b)
			return
		}

		// Store a KeyLabelSpatialMap key (index = b + s)
		binary.BigEndian.PutUint64(labelSpatialMapIndex[1:9], b)
		copy(labelSpatialMapIndex[9:9+dvid.IndexZYXSize], zyxBytes)
		key = d.DataKey(op.versionID, dvid.IndexBytes(labelSpatialMapIndex))
		if err = db.Put(key, emptyValue); err != nil {
			dvid.Log(dvid.Normal, "Error on PUT of KeyLabelSpatialMap: %d + %s\n", b, dataKey.Index)
			return
		}
	}

	// Notify the requestor that this chunk is done.
	if chunk.Wg != nil {
		chunk.Wg.Done()
	}
}