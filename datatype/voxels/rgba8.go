/*
	Data type rgba8 tailors the voxels data type for 8-bit RGBA images.  It simply
	wraps the voxels package, setting Channels (4) and BytesPerValue(1).
*/

package voxels

import (
	"github.com/janelia-flyem/dvid/datastore"
)

func init() {
	values := DataValues{
		{
			DataType: "uint8",
			Label:    "red",
		},
		{
			DataType: "uint8",
			Label:    "green",
		},
		{
			DataType: "uint8",
			Label:    "blue",
		},
		{
			DataType: "uint8",
			Label:    "alpha",
		},
	}
	rgba := NewDatatype(values)
	rgba.DatatypeID = &datastore.DatatypeID{
		Name:    "rgba8",
		Url:     "github.com/janelia-flyem/dvid/datatype/voxels/rgba8.go",
		Version: "0.6",
	}
	datastore.RegisterDatatype(rgba)
}
