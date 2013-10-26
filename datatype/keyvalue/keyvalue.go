/*
	Package keyvalue implements DVID support for data using generic key/value.
*/
package keyvalue

import (
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version = "0.1"
	RepoUrl = "github.com/janelia-flyem/dvid/datatype/keyvalue"
)

const HelpMessage = `
API for 'keyvalue' datatype (github.com/janelia-flyem/dvid/datatype/keyvalue)
=============================================================================

Command-line:

$ dvid dataset <UUID> new keyvalue <data name> <settings...>

	Adds newly named key/value data to dataset with specified UUID.

	Example:

	$ dvid dataset 3f8c new keyvalue stuff

    Arguments:

    UUID           Hexidecimal string with enough characters to uniquely identify a version node.
    type name      Data type name, e.g., "grayscale8"
    data name      Name of data to create, e.g., "mygrayscale"
    settings       Configuration settings in "key=value" format separated by spaces.

    Configuration Settings (case-insensitive keys)

    Versioned      "true" or "false" (default)

$ dvid node <UUID> <data name> get <key>

    Returns data for a key in the given version node.  Since the returned data is
    binary, the user typically pipes the output to a file.

    Example: 

    $ dvid node 3f8c stuff get mykey > myvalue

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    key           A string key.
	
$ dvid node <UUID> <data name> put local  <key> <file name>
$ dvid node <UUID> <data name> put remote <key> <file name>    (TODO -- currently unimplemented)

    Adds file data to a version node when the server can see the local files ("local")
    or when the server must be sent the files via rpc ("remote").

    Example: 

    $ dvid node 3f8c stuff put local stuff.txt

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    key           A string key.
    file name     File name of the value to be stored.
	
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
    data name     Name of voxels data.


GET  /api/node/<UUID>/<data name>/<key>[/<format>]
POST /api/node/<UUID>/<data name>/<key>
DEL  /api/node/<UUID>/<data name>/<key>  (TO DO)

    Retrieves or puts values given a key.  POSTed data should use the "data" key in a form.

    Example: 

    GET /api/node/3f8c/stuff/mykey

    Returns the data associated with the key "mykey" of the data "stuff" in version
    node 3f8c.

    The "Content-type" of the HTTP response should agree with the requested format.

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add/retrieve.
    key           An alphanumeric key.
    format        Valid formats:
                  "octet" (default) returns "application/octet-stream" that RFC2046 says is
                    used for arbitrary binary data.  Clients will usually store this returned
                    value into a file.
`

func init() {
	kvtype := NewDatatype()
	kvtype.DatatypeID = &datastore.DatatypeID{
		Name:    "keyvalue",
		Url:     RepoUrl,
		Version: Version,
	}
	datastore.RegisterDatatype(kvtype)

	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Datatype{})
	gob.Register(&Data{})
	gob.Register(&binary.LittleEndian)
	gob.Register(&binary.BigEndian)
}

// Datatype embeds the datastore's Datatype to create a unique type for keyvalue functions.
type Datatype struct {
	datastore.Datatype
}

// NewDatatype returns a pointer to a new keyvalue Datatype with default values set.
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

// NewData returns a pointer to new keyvalue data with default values.
func (dtype *Datatype) NewDataService(id *datastore.DataID, c dvid.Config) (datastore.DataService, error) {
	basedata, err := datastore.NewDataService(id, dtype, c)
	if err != nil {
		return nil, err
	}
	return &Data{Data: basedata}, nil
}

func (dtype *Datatype) Help() string {
	return fmt.Sprintf(HelpMessage)
}

// Data embeds the datastore's Data and extends it with keyvalue properties (none for now).
type Data struct {
	*datastore.Data
}

// GetData gets a value using a key at a given uuid
func (d *Data) GetData(uuid datastore.UUID, keyStr string) ([]byte, error) {
	// Compute the key
	versionID, err := server.VersionLocalID(uuid)
	if err != nil {
		return nil, err
	}
	key := d.DataKey(versionID, dvid.IndexString(keyStr))

	// Get the data
	db := server.StorageEngine()
	data, err := db.Get(key)
	uncompress := true
	value, _, err := dvid.DeserializeData(data, uncompress)
	if err != nil {
		return nil, fmt.Errorf("Unable to deserialize data for key '%s': %s\n", keyStr, err.Error())
	}
	return value, nil
}

// PutData puts a key/value at a given uuid
func (d *Data) PutData(uuid datastore.UUID, keyStr string, value []byte) error {
	// Compute the key
	versionID, err := server.VersionLocalID(uuid)
	if err != nil {
		return err
	}
	key := d.DataKey(versionID, dvid.IndexString(keyStr))

	// PUT the file
	db := server.StorageEngine()
	serialization, err := dvid.SerializeData(value, dvid.Snappy, dvid.CRC32)
	if err != nil {
		return fmt.Errorf("Unable to serialize data: %s\n", err.Error())
	}
	return db.Put(key, serialization)
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
	case "get":
		return d.Get(request, reply)
	case "put":
		switch request.Command[4] {
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
func (d *Data) DoHTTP(uuid datastore.UUID, w http.ResponseWriter, r *http.Request) error {
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
	keyStr := parts[3]
	switch strings.ToLower(r.Method) {
	case "get":
		value, err := d.GetData(uuid, keyStr)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, err = w.Write(value)
		if err != nil {
			return err
		}
		comment = fmt.Sprintf("Returned %d bytes for key '%s', data '%s', uuid %s\n",
			len(value), keyStr, d.DataName(), uuid)
	case "post":
		data, err := dvid.DataFromPost(r, "data")
		if err != nil {
			return err
		}
		err = d.PutData(uuid, keyStr, data)
		if err != nil {
			return err
		}
		comment = fmt.Sprintf("POST %d bytes for data %s: key '%s', uuid %s\n",
			len(data), d.DataName(), keyStr, uuid)
	default:
		return fmt.Errorf("Can only handle GET or POST HTTP verbs")
	}

	dvid.ElapsedTime(dvid.Debug, startTime, comment)
	return nil
}

// Get retrieves data given a key and a version node.
func (d *Data) Get(request datastore.Request, reply *datastore.Response) error {
	startTime := time.Now()

	// Parse the request
	var uuidStr, dataName, cmdStr, keyStr string
	request.CommandArgs(1, &uuidStr, &dataName, &cmdStr, &keyStr)

	// Put the data
	uuid, err := server.MatchingUUID(uuidStr)
	if err != nil {
		return err
	}
	data, err := d.GetData(uuid, keyStr)
	if err != nil {
		return err
	}
	reply.Text = string(data)
	dvid.ElapsedTime(dvid.Debug, startTime, "RPC GET (%s) completed", keyStr)
	return nil
}

// PutLocal puts file data data to a version node.
func (d *Data) PutLocal(request datastore.Request, reply *datastore.Response) error {
	startTime := time.Now()

	// Parse the request
	var uuidStr, dataName, cmdStr, sourceStr, keyStr string
	filenames := request.CommandArgs(1, &uuidStr, &dataName, &cmdStr, &sourceStr, &keyStr)
	if len(filenames) == 0 {
		return fmt.Errorf("Need to include at least one file to add: %s", request)
	}
	if len(filenames) > 1 {
		return fmt.Errorf("keyvalue loads can only take one file at this time")
	}
	filename := filenames[0]

	// Load the file
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return err
	}

	// Put the data
	uuid, err := server.MatchingUUID(uuidStr)
	if err != nil {
		return err
	}
	err = d.PutData(uuid, keyStr, data)
	dvid.ElapsedTime(dvid.Debug, startTime, "RPC put local of file (%s) completed", filename)
	return err
}