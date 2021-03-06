/*
	This file provides the highest-level view of the datastore via a Service.
*/

package datastore

import (
	"encoding/json"
	"fmt"

	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version = "0.7"
)

// Versions returns a chart of version identifiers for data types and and DVID's datastore
// fixed at compile-time for this DVID executable
func Versions() string {
	var text string = "\nCompile-time version information for this DVID executable:\n\n"
	writeLine := func(name, version string) {
		text += fmt.Sprintf("%-15s   %s\n", name, version)
	}
	writeLine("Name", "Version")
	writeLine("DVID datastore", Version)
	writeLine("Storage driver", storage.Version)
	for _, datatype := range CompiledTypes {
		writeLine(datatype.DatatypeName(), datatype.DatatypeVersion())
	}
	return text
}

// Init creates a key-value datastore using default arguments.  Datastore
// configuration is stored as key/values in the datastore and also in a
// human-readable config file in the datastore directory.
func Init(directory string, create bool, config dvid.Config) error {
	fmt.Println("\nInitializing datastore at", directory)

	// Initialize the backend database
	db, err := storage.NewStore(directory, create, config)
	if err != nil {
		return fmt.Errorf("Error initializing datastore (%s): %s\n", directory, err.Error())
	}
	defer db.Close()

	// Put empty Datasets
	datasets := new(Datasets)
	err = datasets.Put(db)
	return err
}

// Service couples an open DVID storage engine and DVID datasets.
type Service struct {
	datasets *Datasets

	// The backend storage which is private since we want to create an object
	// interface (e.g., cache object or UUID map) and hide DVID-specific keys.
	db storage.Engine
}

type OpenErrorType int

const (
	ErrorOpening OpenErrorType = iota
	ErrorDatasets
	ErrorDatatypeUnavailable
)

type OpenError struct {
	error
	ErrorType OpenErrorType
}

// Open opens a DVID datastore at the given path (directory, url, etc) and returns
// a Service that allows operations on that datastore.
func Open(path string) (s *Service, openErr *OpenError) {
	// Open the datastore
	create := false
	db, err := storage.NewStore(path, create, dvid.Config{})
	if err != nil {
		openErr = &OpenError{
			fmt.Errorf("Error opening datastore (%s): %s", path, err.Error()),
			ErrorOpening,
		}
		return
	}

	// Read this datastore's configuration
	datasets := new(Datasets)
	err = datasets.Load(db)
	if err != nil {
		openErr = &OpenError{
			fmt.Errorf("Error reading datasets: %s", err.Error()),
			ErrorDatasets,
		}
		return
	}

	// Verify that the runtime configuration can be supported by this DVID's
	// compiled-in data types.
	dvid.Fmt(dvid.Debug, "Verifying datastore's supported types were compiled into DVID...\n")
	err = datasets.VerifyCompiledTypes()
	if err != nil {
		openErr = &OpenError{
			fmt.Errorf("Data are not fully supported by this DVID server: %s", err.Error()),
			ErrorDatatypeUnavailable,
		}
		return
	}

	fmt.Printf("\nDatastoreService successfully opened: %s\n", path)
	s = &Service{datasets, db}
	return
}

// Shutdown closes a DVID datastore.
func (s *Service) Shutdown() {
	s.db.Close()
}

// DatasetsListJSON returns JSON of a list of datasets.
func (s *Service) DatasetsListJSON() (stringJSON string, err error) {
	if s.datasets == nil {
		stringJSON = "{}"
		return
	}
	var bytesJSON []byte
	bytesJSON, err = s.datasets.MarshalJSON()
	if err != nil {
		return
	}
	return string(bytesJSON), nil
}

// DatasetsAllJSON returns JSON of a list of datasets.
func (s *Service) DatasetsAllJSON() (stringJSON string, err error) {
	if s.datasets == nil {
		stringJSON = "{}"
		return
	}
	var bytesJSON []byte
	bytesJSON, err = s.datasets.AllJSON()
	if err != nil {
		return
	}
	return string(bytesJSON), nil
}

// DatasetJSON returns JSON for a particular dataset referenced by a uuid.
func (s *Service) DatasetJSON(root dvid.UUID) (stringJSON string, err error) {
	if s.datasets == nil {
		stringJSON = "{}"
		return
	}
	dataset, err := s.datasets.DatasetFromUUID(root)
	if err != nil {
		return "{}", err
	}
	stringJSON, err = dataset.JSONString()
	return
}

// NOTE: Alterations of Datasets should invoke persistence to the key-value database.
// All interaction with datasets at the datastore.Service level should be using
// opaque UUID or the shortened datasetID.

// NewDataset creates a new dataset.
func (s *Service) NewDataset() (root dvid.UUID, datasetID dvid.DatasetLocalID, err error) {
	if s.datasets == nil {
		err = fmt.Errorf("Datastore service has no datasets available")
		return
	}
	var dataset *Dataset
	dataset, err = s.datasets.newDataset()
	if err != nil {
		return
	}
	err = s.datasets.Put(s.db) // Need to persist change to list of Dataset
	if err != nil {
		return
	}
	err = dataset.Put(s.db)
	root = dataset.Root
	datasetID = dataset.DatasetID
	return
}

// NewVersions creates a new version (child node) off of a LOCKED parent node.
// Will return an error if the parent node has not been locked.
func (s *Service) NewVersion(parent dvid.UUID) (u dvid.UUID, err error) {
	if s.datasets == nil {
		err = fmt.Errorf("Datastore service has no datasets available")
		return
	}
	var dataset *Dataset
	dataset, u, err = s.datasets.newChild(parent)
	if err != nil {
		return
	}
	err = dataset.Put(s.db)
	return
}

// NewData adds data of given name and type to a dataset specified by a UUID.
func (s *Service) NewData(u dvid.UUID, typename, dataname string, config dvid.Config) error {
	if s.datasets == nil {
		return fmt.Errorf("Datastore service has no datasets available")
	}
	dataset, err := s.datasets.DatasetFromUUID(u)
	if err != nil {
		return err
	}
	err = dataset.newData(dvid.DataString(dataname), typename, config)
	if err != nil {
		return err
	}
	return dataset.Put(s.db)
}

// Locks the node with the given UUID.
func (s *Service) Lock(u dvid.UUID) error {
	if s.datasets == nil {
		return fmt.Errorf("Datastore service has no datasets available")
	}
	dataset, err := s.datasets.DatasetFromUUID(u)
	if err != nil {
		return err
	}
	err = dataset.Lock(u)
	if err != nil {
		return err
	}
	return dataset.Put(s.db)
}

// SaveDataset forces this service to persist the dataset with given UUID.
// It is useful when modifying datasets internally.
func (s *Service) SaveDataset(u dvid.UUID) error {
	if s.datasets == nil {
		return fmt.Errorf("Datastore service has no datasets available")
	}
	dataset, err := s.datasets.DatasetFromUUID(u)
	if err != nil {
		return err
	}
	return dataset.Put(s.db)
}

// LocalIDFromUUID when supplied a UUID string, returns smaller sized local IDs that identify a
// dataset and a version.
func (s *Service) LocalIDFromUUID(u dvid.UUID) (dID dvid.DatasetLocalID, vID dvid.VersionLocalID, err error) {
	if s.datasets == nil {
		err = fmt.Errorf("Datastore service has no datasets available")
		return
	}
	var dataset *Dataset
	dataset, err = s.datasets.DatasetFromUUID(u)
	if err != nil {
		return
	}
	dID = dataset.DatasetID
	var found bool
	vID, found = dataset.VersionMap[u]
	if !found {
		err = fmt.Errorf("UUID (%s) not found in dataset", u)
	}
	return
}

// NodeIDFromString when supplied a UUID string, returns the matched UUID as well as
// more compact local IDs that identify the dataset and a version.  Partial matches
// are allowed, similar to DatasetFromString.
func (s *Service) NodeIDFromString(str string) (u dvid.UUID, dID dvid.DatasetLocalID,
	vID dvid.VersionLocalID, err error) {

	if s.datasets == nil {
		err = fmt.Errorf("Datastore service has no datasets available")
		return
	}
	var dataset *Dataset
	dataset, u, err = s.datasets.DatasetFromString(str)
	if err != nil {
		return
	}
	dID = dataset.DatasetID
	vID = dataset.VersionMap[u]
	return
}

// DataService returns a service for data of a given name and version
func (s *Service) DataService(u dvid.UUID, name dvid.DataString) (dataservice DataService, err error) {
	if s.datasets == nil {
		err = fmt.Errorf("Datastore service has no datasets available")
		return
	}
	return s.datasets.DataService(u, name)
}

// StorageEngine returns a a key-value database interface.
func (s *Service) StorageEngine() storage.Engine {
	return s.db
}

// Batcher returns an interface that can create a new batch write.
func (s *Service) Batcher() (db storage.Batcher, err error) {
	if s.db.IsBatcher() {
		var ok bool
		db, ok = s.db.(storage.Batcher)
		if !ok {
			err = fmt.Errorf("DVID backend says it supports batch write but does not!")
		}
	} else {
		err = fmt.Errorf("DVID backend database does not support batch write")
	}
	return
}

// SupportedDataChart returns a chart (names/urls) of data referenced by this datastore
func (s *Service) SupportedDataChart() string {
	text := CompiledTypeChart()
	text += "Data currently referenced within this DVID datastore:\n\n"
	text += s.DataChart()
	return text
}

// About returns a chart of the code versions of compile-time DVID datastore
// and the runtime data types.
func (s *Service) About() string {
	var text string
	writeLine := func(name, version string) {
		text += fmt.Sprintf("%-15s   %s\n", name, version)
	}
	writeLine("Name", "Version")
	writeLine("DVID datastore", Version)
	writeLine("Storage backend", storage.Version)
	if s.datasets != nil {
		for _, dtype := range s.datasets.Datatypes() {
			writeLine(dtype.DatatypeName(), dtype.DatatypeVersion())
		}
	}
	return text
}

// TypesJSON returns the components and versions of DVID software available
// in this DVID server.
func (s *Service) TypesJSON() (jsonStr string, err error) {
	data := make(map[string]string)
	for _, datatype := range CompiledTypes {
		data[datatype.DatatypeName()] = string(datatype.DatatypeUrl())
	}
	m, err := json.Marshal(data)
	if err != nil {
		return
	}
	jsonStr = string(m)
	return
}

// CurrentTypesJSON returns the components and versions of DVID software associated
// with the current datasets in the service.
func (s *Service) CurrentTypesJSON() (jsonStr string, err error) {
	data := make(map[string]string)
	if s.datasets != nil {
		for _, dtype := range s.datasets.Datatypes() {
			data[dtype.DatatypeName()] = dtype.DatatypeVersion()
		}
	}
	m, err := json.Marshal(data)
	if err != nil {
		return
	}
	jsonStr = string(m)
	return
}

// DataChart returns a text chart of data names and their types for this DVID server.
func (s *Service) DataChart() string {
	var text string
	if s.datasets == nil || len(s.datasets.list) == 0 {
		return "  No datasets have been added to this datastore.\n"
	}
	writeLine := func(name dvid.DataString, version string, url UrlString) {
		text += fmt.Sprintf("%-15s  %-25s  %s\n", name, version, url)
	}
	for num, dset := range s.datasets.list {
		text += fmt.Sprintf("\nDataset %d (UUID = %s):\n\n", num+1, dset.Root)
		writeLine("Name", "Type Name", "Url")
		for name, data := range dset.DataMap {
			writeLine(name, data.DatatypeName()+" ("+data.DatatypeVersion()+")",
				data.DatatypeUrl())
		}
	}
	return text
}
